package config

import (
	"testing"

	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	swarmt "github.com/libp2p/go-libp2p-swarm/testing"
	bhost "github.com/tonyHup/go-libp2p/p2p/host/basic"

	"github.com/libp2p/go-libp2p-core/mux"
	yamux "github.com/libp2p/go-libp2p-yamux"
)

func TestMuxerSimple(t *testing.T) {
	// single
	_, err := MuxerConstructor(func(_ peer.ID) mux.Multiplexer { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestMuxerByValue(t *testing.T) {
	_, err := MuxerConstructor(yamux.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
}
func TestMuxerDuplicate(t *testing.T) {
	_, err := MuxerConstructor(func(_ peer.ID, _ peer.ID) mux.Multiplexer { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestMuxerError(t *testing.T) {
	_, err := MuxerConstructor(func() (mux.Multiplexer, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestMuxerBadTypes(t *testing.T) {
	for i, f := range []interface{}{
		func() error { return nil },
		func() string { return "" },
		func() {},
		func(string) mux.Multiplexer { return nil },
		func(string) (mux.Multiplexer, error) { return nil, nil },
		nil,
		"testing",
	} {

		if _, err := MuxerConstructor(f); err == nil {
			t.Fatalf("constructor %d with type %T should have failed", i, f)
		}
	}
}

func TestCatchDuplicateTransportsMuxer(t *testing.T) {
	h, err := bhost.NewHost(swarmt.GenSwarm(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	yamuxMuxer, err := MuxerConstructor(yamux.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}

	var tests = map[string]struct {
		h             host.Host
		transports    []MsMuxC
		expectedError string
	}{
		"no duplicate transports": {
			h:             h,
			transports:    []MsMuxC{{yamuxMuxer, "yamux"}},
			expectedError: "",
		},
		"duplicate transports": {
			h: h,
			transports: []MsMuxC{
				{yamuxMuxer, "yamux"},
				{yamuxMuxer, "yamux"},
			},
			expectedError: "duplicate muxer transport: yamux",
		},
	}
	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			_, err = makeMuxer(test.h, test.transports)
			if err != nil {
				if err.Error() != test.expectedError {
					t.Errorf(
						"\nexpected: [%v]\nactual:   [%v]\n",
						test.expectedError,
						err,
					)
				}
			}
		})
	}
}
