package activator

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivator(t *testing.T) {
	netNS, err := ns.GetCurrentNS()
	require.NoError(t, err)

	tests := map[string]struct {
		netLocker NetworkLocker
	}{
		"iptables netlocker": {
			netLocker: &iptablesLocker{netNS: netNS},
		},
		"nftables netlocker": {
			netLocker: &nftablesLocker{netNS: netNS},
		},
	}

	for name, tc := range tests {
		tc := tc
		t.Run(name, func(t *testing.T) {
			if tc.netLocker.Backend() == NetworkLockerNFTables && !nftablesSupported() {
				t.Skip("skipping nftables test as system does not support it")
			}

			ctx, cancel := context.WithCancel(context.Background())

			port, port2, err := getFreePorts()
			require.NoError(t, err)

			s, err := NewServer(ctx, []uint16{uint16(port), uint16(port2)}, netNS, tc.netLocker)
			require.NoError(t, err)

			response := "ok"
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, response)
			}))
			ts2 := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, response)
			}))

			if err := s.Start(ctx,
				func() error {
					l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
					if err != nil {
						log.Fatal(err)
					}

					l2, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port2))
					if err != nil {
						log.Fatal(err)
					}

					// NewUnstartedServer creates a listener. Close that listener and replace
					// with the one we created.
					ts.Listener.Close()
					ts.Listener = l
					ts.Start()

					ts2.Listener.Close()
					ts2.Listener = l2
					ts2.Start()

					t.Cleanup(func() {
						ts.Close()
						ts2.Close()
					})

					return nil
				},
				func() error {
					return nil
				},
			); err != nil {
				t.Fatal(err)
			}

			defer s.Stop(ctx)
			defer cancel()

			c := &http.Client{Timeout: time.Second}

			parallelReqs := 6
			wg := sync.WaitGroup{}
			for _, port := range []int{port, port2} {
				port := port
				for i := 0; i < parallelReqs; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						resp, err := c.Get(fmt.Sprintf("http://localhost:%d", port))
						require.NoError(t, err)
						b, err := io.ReadAll(resp.Body)
						require.NoError(t, err)

						assert.Equal(t, response, string(b))
						t.Log(string(b))
					}()
				}
			}
			wg.Wait()
		})
	}
}

func getFreePorts() (int, int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, 0, err
	}
	listener2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, 0, err
	}

	port := listener.Addr().(*net.TCPAddr).Port
	port2 := listener2.Addr().(*net.TCPAddr).Port

	if err := listener.Close(); err != nil {
		return 0, 0, err
	}

	if err := listener2.Close(); err != nil {
		return 0, 0, err
	}

	return port, port2, nil
}
