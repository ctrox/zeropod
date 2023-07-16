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

	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivator(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	netNS, err := ns.GetCurrentNS()
	require.NoError(t, err)

	port, err := getFreePort()
	require.NoError(t, err)

	s, err := NewServer(ctx, uint16(port), netNS, NewNetworkLocker(netNS))
	require.NoError(t, err)

	response := "ok"
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, response)
	}))

	if err := s.Start(ctx,
		func() (*runc.Container, error) {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				log.Fatal(err)
			}

			// NewUnstartedServer creates a listener. Close that listener and replace
			// with the one we created.
			ts.Listener.Close()
			ts.Listener = l
			ts.Start()

			t.Cleanup(func() {
				ts.Close()
			})

			return nil, nil
		},
		func(c *runc.Container) error {
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
	wg.Wait()
}

func getFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	port := listener.Addr().(*net.TCPAddr).Port
	err = listener.Close()
	if err != nil {
		return 0, err
	}

	return port, nil
}
