package holepunch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	pb "github.com/tonyHup/go-libp2p/p2p/protocol/holepunch/pb"
	"github.com/tonyHup/go-libp2p/p2p/protocol/identify"

	logging "github.com/ipfs/go-log/v2"
	"github.com/libp2p/go-msgio/protoio"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// Protocol is the libp2p protocol for Hole Punching.
const Protocol protocol.ID = "/libp2p/dcutr"

// StreamTimeout is the timeout for the hole punch protocol stream.
var StreamTimeout = 1 * time.Minute

// TODO Should we have options for these ?
const (
	maxMsgSize  = 4 * 1024 // 4K
	dialTimeout = 5 * time.Second
	maxRetries  = 3
	retryWait   = 2 * time.Second
)

var (
	log = logging.Logger("p2p-holepunch")
	// ErrHolePunchActive is returned from DirectConnect when another hole punching attempt is currently running
	ErrHolePunchActive = errors.New("another hole punching attempt to this peer is active")
	// ErrClosed is returned when the hole punching is closed
	ErrClosed = errors.New("hole punching service closing")
)

// The Service is used to make direct connections with a peer via hole-punching.
type Service struct {
	ctx       context.Context
	ctxCancel context.CancelFunc

	ids  *identify.IDService
	host host.Host

	tracer *tracer

	closeMx  sync.RWMutex
	closed   bool
	refCount sync.WaitGroup

	// active hole punches for deduplicating
	activeMx sync.Mutex
	active   map[peer.ID]struct{}
}

type Option func(*Service) error

// NewService creates a new service that can be used for hole punching
func NewService(h host.Host, ids *identify.IDService, opts ...Option) (*Service, error) {
	if ids == nil {
		return nil, errors.New("identify service can't be nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	hs := &Service{
		ctx:       ctx,
		ctxCancel: cancel,
		host:      h,
		ids:       ids,
		active:    make(map[peer.ID]struct{}),
	}

	for _, opt := range opts {
		if err := opt(hs); err != nil {
			cancel()
			return nil, err
		}
	}

	h.SetStreamHandler(Protocol, hs.handleNewStream)
	h.Network().Notify((*netNotifiee)(hs))
	return hs, nil
}

// Close closes the Hole Punch Service.
func (hs *Service) Close() error {
	hs.closeMx.Lock()
	hs.closed = true
	hs.closeMx.Unlock()
	hs.tracer.Close()
	hs.host.RemoveStreamHandler(Protocol)
	hs.ctxCancel()
	hs.refCount.Wait()
	return nil
}

// initiateHolePunch opens a new hole punching coordination stream,
// exchanges the addresses and measures the RTT.
func (hs *Service) initiateHolePunch(rp peer.ID) ([]ma.Multiaddr, time.Duration, error) {
	hpCtx := network.WithUseTransient(hs.ctx, "hole-punch")
	sCtx := network.WithNoDial(hpCtx, "hole-punch")
	str, err := hs.host.NewStream(sCtx, rp, Protocol)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open hole-punching stream: %w", err)
	}
	defer str.Close()
	str.SetDeadline(time.Now().Add(StreamTimeout))

	w := protoio.NewDelimitedWriter(str)

	// send a CONNECT and start RTT measurement.
	msg := &pb.HolePunch{
		Type:     pb.HolePunch_CONNECT.Enum(),
		ObsAddrs: addrsToBytes(hs.ids.OwnObservedAddrs()),
	}

	start := time.Now()
	if err := w.WriteMsg(msg); err != nil {
		str.Reset()
		return nil, 0, err
	}

	// wait for a CONNECT message from the remote peer
	rd := protoio.NewDelimitedReader(str, maxMsgSize)
	msg.Reset()
	if err := rd.ReadMsg(msg); err != nil {
		str.Reset()
		return nil, 0, fmt.Errorf("failed to read CONNECT message from remote peer: %w", err)
	}
	rtt := time.Since(start)

	if t := msg.GetType(); t != pb.HolePunch_CONNECT {
		str.Reset()
		return nil, 0, fmt.Errorf("expect CONNECT message, got %s", t)
	}

	addrs := addrsFromBytes(msg.ObsAddrs)

	msg.Reset()
	msg.Type = pb.HolePunch_SYNC.Enum()
	if err := w.WriteMsg(msg); err != nil {
		str.Reset()
		return nil, 0, fmt.Errorf("failed to send SYNC message for hole punching: %w", err)
	}
	return addrs, rtt, nil
}

func (hs *Service) beginDirectConnect(p peer.ID) error {
	hs.closeMx.RLock()
	defer hs.closeMx.RUnlock()
	if hs.closed {
		return ErrClosed
	}

	hs.activeMx.Lock()
	defer hs.activeMx.Unlock()
	if _, ok := hs.active[p]; ok {
		return ErrHolePunchActive
	}

	hs.active[p] = struct{}{}
	return nil
}

// DirectConnect attempts to make a direct connection with a remote peer.
// It first attempts a direct dial (if we have a public address of that peer), and then
// coordinates a hole punch over the given relay connection.
func (hs *Service) DirectConnect(p peer.ID) error {
	log.Debugw("got inbound proxy conn", "peer", p)
	if err := hs.beginDirectConnect(p); err != nil {
		return err
	}

	defer func() {
		hs.activeMx.Lock()
		delete(hs.active, p)
		hs.activeMx.Unlock()
	}()

	return hs.directConnect(p)
}

func (hs *Service) directConnect(rp peer.ID) error {
	// short-circuit check to see if we already have a direct connection
	for _, c := range hs.host.Network().ConnsToPeer(rp) {
		if !isRelayAddress(c.RemoteMultiaddr()) {
			return nil
		}
	}

	// short-circuit hole punching if a direct dial works.
	// attempt a direct connection ONLY if we have a public address for the remote peer
	for _, a := range hs.host.Peerstore().Addrs(rp) {
		if manet.IsPublicAddr(a) && !isRelayAddress(a) {
			forceDirectConnCtx := network.WithForceDirectDial(hs.ctx, "hole-punching")
			dialCtx, cancel := context.WithTimeout(forceDirectConnCtx, dialTimeout)

			tstart := time.Now()
			// This dials *all* public addresses from the peerstore.
			err := hs.host.Connect(dialCtx, peer.AddrInfo{ID: rp})
			dt := time.Since(tstart)
			cancel()

			if err != nil {
				hs.tracer.DirectDialFailed(rp, dt, err)
				break
			}
			hs.tracer.DirectDialSuccessful(rp, dt)
			log.Debugw("direct connection to peer successful, no need for a hole punch", "peer", rp)
			return nil
		}
	}

	// hole punch
	for i := 0; i < maxRetries; i++ {
		addrs, rtt, err := hs.initiateHolePunch(rp)
		if err != nil {
			log.Debugw("hole punching failed", "peer", rp, "error", err)
			hs.tracer.ProtocolError(rp, err)
			return err
		}
		synTime := rtt / 2
		log.Debugf("peer RTT is %s; starting hole punch in %s", rtt, synTime)

		// wait for sync to reach the other peer and then punch a hole for it in our NAT
		// by attempting a connect to it.
		timer := time.NewTimer(synTime)
		select {
		case start := <-timer.C:
			pi := peer.AddrInfo{
				ID:    rp,
				Addrs: addrs,
			}
			hs.tracer.StartHolePunch(rp, addrs, rtt)
			err := hs.holePunchConnect(pi, true)
			dt := time.Since(start)
			hs.tracer.EndHolePunch(rp, dt, err)
			if err == nil {
				log.Debugw("hole punching with successful", "peer", rp, "time", dt)
				return nil
			}
		case <-hs.ctx.Done():
			timer.Stop()
			return hs.ctx.Err()
		}
	}
	return fmt.Errorf("all retries for hole punch with peer %s failed", rp)
}

func (hs *Service) incomingHolePunch(s network.Stream) (rtt time.Duration, addrs []ma.Multiaddr, err error) {
	// sanity check: a hole punch request should only come from peers behind a relay
	if !isRelayAddress(s.Conn().RemoteMultiaddr()) {
		return 0, nil, fmt.Errorf("received hole punch stream: %s", s.Conn().RemoteMultiaddr())
	}

	s.SetDeadline(time.Now().Add(StreamTimeout))
	wr := protoio.NewDelimitedWriter(s)
	rd := protoio.NewDelimitedReader(s, maxMsgSize)

	// Read Connect message
	msg := new(pb.HolePunch)
	if err := rd.ReadMsg(msg); err != nil {
		return 0, nil, fmt.Errorf("failed to read message from initator: %w", err)
	}
	if t := msg.GetType(); t != pb.HolePunch_CONNECT {
		return 0, nil, fmt.Errorf("expected CONNECT message from initiator but got %d", t)
	}
	obsDial := addrsFromBytes(msg.ObsAddrs)
	log.Debugw("received hole punch request", "peer", s.Conn().RemotePeer(), "addrs", obsDial)

	// Write CONNECT message
	msg.Reset()
	msg.Type = pb.HolePunch_CONNECT.Enum()
	msg.ObsAddrs = addrsToBytes(hs.ids.OwnObservedAddrs())
	tstart := time.Now()
	if err := wr.WriteMsg(msg); err != nil {
		return 0, nil, fmt.Errorf("failed to write CONNECT message to initator: %w", err)
	}

	// Read SYNC message
	msg.Reset()
	if err := rd.ReadMsg(msg); err != nil {
		return 0, nil, fmt.Errorf("failed to read message from initator: %w", err)
	}
	if t := msg.GetType(); t != pb.HolePunch_SYNC {
		return 0, nil, fmt.Errorf("expected SYNC message from initiator but got %d", t)
	}
	return time.Since(tstart), obsDial, nil
}

func (hs *Service) handleNewStream(s network.Stream) {
	// Check directionality of the underlying connection.
	// Peer A receives an inbound connection from peer B.
	// Peer A opens a new hole punch stream to peer B.
	// Peer B receives this stream, calling this function.
	// Peer B sees the underlying connection as an outbound connection.
	if s.Conn().Stat().Direction == network.DirInbound {
		s.Reset()
		return
	}
	rp := s.Conn().RemotePeer()
	rtt, addrs, err := hs.incomingHolePunch(s)
	if err != nil {
		hs.tracer.ProtocolError(rp, err)
		log.Debugw("error handling holepunching stream from", rp, "error", err)
		s.Reset()
		return
	}
	s.Close()

	// Hole punch now by forcing a connect
	pi := peer.AddrInfo{
		ID:    rp,
		Addrs: addrs,
	}
	hs.tracer.StartHolePunch(rp, addrs, rtt)
	log.Debugw("starting hole punch", "peer", rp)
	start := time.Now()
	err = hs.holePunchConnect(pi, false)
	dt := time.Since(start)
	hs.tracer.EndHolePunch(rp, dt, err)
	if err != nil {
		log.Debugw("hole punching failed", "peer", rp, "time", dt, "error", err)
	} else {
		log.Debugw("hole punching succeeded", "peer", rp, "time", dt)
	}
}

func (hs *Service) holePunchConnect(pi peer.AddrInfo, isClient bool) error {
	holePunchCtx := network.WithSimultaneousConnect(hs.ctx, isClient, "hole-punching")
	forceDirectConnCtx := network.WithForceDirectDial(holePunchCtx, "hole-punching")
	dialCtx, cancel := context.WithTimeout(forceDirectConnCtx, dialTimeout)
	defer cancel()

	hs.tracer.HolePunchAttempt(pi.ID)
	if err := hs.host.Connect(dialCtx, pi); err != nil {
		log.Debugw("hole punch attempt with peer failed", "peer ID", pi.ID, "error", err)
		return err
	}
	log.Debugw("hole punch successful", "peer", pi.ID)
	return nil
}

func isRelayAddress(a ma.Multiaddr) bool {
	_, err := a.ValueForProtocol(ma.P_CIRCUIT)
	return err == nil
}

func addrsToBytes(as []ma.Multiaddr) [][]byte {
	bzs := make([][]byte, 0, len(as))
	for _, a := range as {
		bzs = append(bzs, a.Bytes())
	}
	return bzs
}

func addrsFromBytes(bzs [][]byte) []ma.Multiaddr {
	addrs := make([]ma.Multiaddr, 0, len(bzs))
	for _, bz := range bzs {
		a, err := ma.NewMultiaddrBytes(bz)
		if err == nil {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

type netNotifiee Service

func (nn *netNotifiee) Connected(_ network.Network, conn network.Conn) {
	hs := (*Service)(nn)

	// Hole punch if it's an inbound proxy connection.
	// If we already have a direct connection with the remote peer, this will be a no-op.
	if conn.Stat().Direction == network.DirInbound && isRelayAddress(conn.RemoteMultiaddr()) {
		hs.refCount.Add(1)
		go func() {
			defer hs.refCount.Done()

			select {
			// waiting for Identify here will allow us to access the peer's public and observed addresses
			// that we can dial to for a hole punch.
			case <-hs.ids.IdentifyWait(conn):
			case <-hs.ctx.Done():
			}

			_ = hs.DirectConnect(conn.RemotePeer())
		}()
	}
}

func (nn *netNotifiee) Disconnected(_ network.Network, v network.Conn)   {}
func (nn *netNotifiee) OpenedStream(n network.Network, v network.Stream) {}
func (nn *netNotifiee) ClosedStream(n network.Network, v network.Stream) {}
func (nn *netNotifiee) Listen(n network.Network, a ma.Multiaddr)         {}
func (nn *netNotifiee) ListenClose(n network.Network, a ma.Multiaddr)    {}
