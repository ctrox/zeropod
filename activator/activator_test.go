package activator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivator(t *testing.T) {
	require.NoError(t, MountBPFFS(BPFFSPath))

	nn, err := ns.GetCurrentNS()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	s, err := NewServer(ctx, nn)
	require.NoError(t, err)

	bpf, err := InitBPF(os.Getpid(), slog.Default())
	require.NoError(t, err)
	require.NoError(t, bpf.AttachRedirector("lo"))

	port, err := freePort()
	require.NoError(t, err)

	t.Cleanup(func() {
		s.Stop(ctx)
		cancel()
	})

	c := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	tests := map[string]struct {
		parallelReqs int
		connHook     ConnHook
		expectedBody string
		expectedCode int
	}{
		"no probe": {
			parallelReqs: 1,
			expectedBody: "ok",
			expectedCode: http.StatusOK,
		},
		"10 in parallel": {
			parallelReqs: 10,
			expectedBody: "ok",
			expectedCode: http.StatusOK,
		},
		"conn hook": {
			parallelReqs: 1,
			expectedBody: "",
			connHook: func(conn net.Conn) (net.Conn, bool, error) {
				resp := http.Response{
					StatusCode: http.StatusForbidden,
				}
				return conn, false, resp.Write(conn)
			},
			expectedCode: http.StatusForbidden,
		},
	}
	wg := sync.WaitGroup{}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			startServer(t, ctx, s, port, tc.connHook)
			for i := 0; i < tc.parallelReqs; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://localhost:%d", port), nil)
					if !assert.NoError(t, err) {
						return
					}

					resp, err := c.Do(req)
					if !assert.NoError(t, err) {
						return
					}

					b, err := io.ReadAll(resp.Body)
					if !assert.NoError(t, err) {
						return
					}

					assert.Equal(t, tc.expectedCode, resp.StatusCode)
					assert.Equal(t, tc.expectedBody, string(b))
					t.Log(string(b))
				}()
			}
			wg.Wait()
			assert.NoError(t, s.Reset())
		})
	}
}

func startServer(t *testing.T, ctx context.Context, s *Server, port int, connHook ConnHook) {
	response := "ok"
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, response)
	}))
	if connHook == nil {
		connHook = func(c net.Conn) (net.Conn, bool, error) {
			return c, true, nil
		}
	}

	once := sync.Once{}
	err := s.Start(
		ctx, []uint16{uint16(port)},
		connHook,
		func() error {
			once.Do(func() {
				// simulate a delay until our server is started
				time.Sleep(time.Millisecond * 200)
				l, err := net.Listen("tcp4", fmt.Sprintf(":%d", port))
				require.NoError(t, err)

				if err := s.DisableRedirects(); err != nil {
					t.Errorf("could not disable redirects: %s", err)
				}

				// replace listener of server
				ts.Listener.Close()
				ts.Listener = l
				ts.Start()
				t.Logf("listening on :%d", port)

				t.Cleanup(func() {
					l.Close()
					ts.Close()
				})
			})
			return nil
		},
	)
	require.NoError(t, err)
}
