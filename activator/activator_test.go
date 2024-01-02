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
	"github.com/ctrox/zeropod/socket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
)

func TestActivator(t *testing.T) {
	t.Skip("broken for the time being")
	require.NoError(t, socket.MountBPFFS(socket.BPFFSPath))

	newns, err := netns.NewNamed("test")
	require.NoError(t, err)
	defer newns.Close()

	nn, err := ns.GetCurrentNS()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	port, _, err := getFreePorts()
	require.NoError(t, err)

	s, err := NewServer(ctx, []uint16{uint16(port)}, nn)
	require.NoError(t, err)

	response := "ok"
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, response)
	}))

	err = s.Start(ctx,
		func() error {
			l, err := net.Listen("tcp4", fmt.Sprintf(":%d", port))
			if err != nil {
				log.Fatal(err)
			}

			// NewUnstartedServer creates a listener. Close that listener and replace
			// with the one we created.
			ts.Listener.Close()
			ts.Listener = l
			ts.Start()
			log.Printf("listening on :%d", port)

			t.Cleanup(func() {
				ts.Close()
			})

			if err := s.DisableRedirects(); err != nil {
				return fmt.Errorf("could not disable redirects: %w", err)
			}

			return nil
		},
	)
	require.NoError(t, err)
	defer s.Stop(ctx)
	defer cancel()

	time.Sleep(time.Hour)

	c := &http.Client{Timeout: time.Second}

	parallelReqs := 6
	wg := sync.WaitGroup{}
	for _, port := range []int{port} {
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
