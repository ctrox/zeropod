package activator

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/socket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivator(t *testing.T) {
	require.NoError(t, socket.MountBPFFS(socket.BPFFSPath))

	nn, err := ns.GetCurrentNS()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	port, err := freePort()
	require.NoError(t, err)

	s, err := NewServer(ctx, nn)
	require.NoError(t, err)

	bpf, err := InitBPF(os.Getpid())
	require.NoError(t, err)
	require.NoError(t, bpf.AttachRedirector("lo"))

	response := "ok"
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, response)
	}))

	err = s.Start(ctx, []uint16{uint16(port)}, func() error {
		// simulate a delay until our server is started
		time.Sleep(time.Millisecond * 200)
		l, err := net.Listen("tcp4", fmt.Sprintf(":%d", port))
		require.NoError(t, err)

		if err := s.DisableRedirects(); err != nil {
			return fmt.Errorf("could not disable redirects: %w", err)
		}

		// replace listener of server
		ts.Listener.Close()
		ts.Listener = l
		ts.Start()
		t.Logf("listening on :%d", port)

		t.Cleanup(func() {
			ts.Close()
		})

		return nil
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		s.Stop(ctx)
		cancel()
	})

	c := &http.Client{Timeout: time.Second}

	parallelReqs := 10
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

				assert.Equal(t, http.StatusOK, resp.StatusCode)
				assert.Equal(t, response, string(b))
				t.Log(string(b))
			}()
		}
	}
	wg.Wait()
}
