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
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testCase struct {
	parallelReqs           int
	connHook               ConnHook
	expectedBody           string
	expectedCode           int
	expectLastActivity     bool
	ipv6                   bool
	setBinaryName          bool
	trackerIgnoreLocalhost bool
}

func TestActivator(t *testing.T) {
	require.NoError(t, MountBPFFS(BPFFSPath))
	nn, err := ns.GetCurrentNS()
	require.NoError(t, err)

	c := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	tests := map[string]testCase{
		"no hook": {
			parallelReqs:       1,
			expectedBody:       "ok",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
		},
		"no hook ipv6": {
			parallelReqs:       1,
			expectedBody:       "ok",
			expectedCode:       http.StatusOK,
			ipv6:               true,
			expectLastActivity: true,
		},
		"10 in parallel": {
			parallelReqs:       10,
			expectedBody:       "ok",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
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
			expectedCode:       http.StatusForbidden,
			expectLastActivity: true,
		},
		"conn hook ipv6": {
			parallelReqs: 1,
			expectedBody: "",
			connHook: func(conn net.Conn) (net.Conn, bool, error) {
				resp := http.Response{
					StatusCode: http.StatusForbidden,
				}
				return conn, false, resp.Write(conn)
			},
			expectedCode:       http.StatusForbidden,
			expectLastActivity: true,
			ipv6:               true,
		},
		"ignore activity with binary name set": {
			parallelReqs:       1,
			expectedBody:       "ok",
			expectedCode:       http.StatusOK,
			setBinaryName:      true,
			expectLastActivity: false,
		},
		"ignore activity with binary name set ipv6": {
			parallelReqs:       1,
			expectedBody:       "ok",
			expectedCode:       http.StatusOK,
			ipv6:               true,
			setBinaryName:      true,
			expectLastActivity: false,
		},
		"ignore activity from localhost v4": {
			parallelReqs:           1,
			expectedBody:           "ok",
			expectedCode:           http.StatusOK,
			ipv6:                   false,
			expectLastActivity:     false,
			trackerIgnoreLocalhost: true,
		},
		"ignore activity from localhost v6": {
			parallelReqs:           1,
			expectedBody:           "ok",
			expectedCode:           http.StatusOK,
			ipv6:                   true,
			expectLastActivity:     false,
			trackerIgnoreLocalhost: true,
		},
	}
	wg := sync.WaitGroup{}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			s, err := NewServer(ctx, nn)
			require.NoError(t, err)

			port, err := freePort()
			require.NoError(t, err)

			t.Cleanup(func() {
				s.Stop(ctx)
				cancel()
			})

			exeName := ""
			if tc.setBinaryName {
				currentExe, err := os.Executable()
				require.NoError(t, err)
				exeName = filepath.Base(currentExe)
			}
			bpf, err := InitBPF(os.Getpid(), slog.Default(),
				ProbeBinaryName(exeName),
				OverrideMapSize(
					// not completely sure why this happens but when testing in
					// github actions, the default map size of 128 makes the test
					// very flaky so we increase it here.
					map[string]uint32{SocketTrackerMap: 1024},
				),
				TrackerIgnoreLocalhost(tc.trackerIgnoreLocalhost),
			)
			require.NoError(t, err)
			require.NoError(t, bpf.AttachRedirector("lo"))

			startServer(t, ctx, s, uint16(port), &tc)
			for i := 0; i < tc.parallelReqs; i++ {
				wg.Go(func() {
					host := "127.0.0.1"
					if tc.ipv6 {
						host = "[::1]"
					}
					req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s:%d", host, port), nil)
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
				})
			}
			wg.Wait()
			var key uint16
			var val uint64
			count := 0
			iter := s.maps.SocketTracker.Iterate()
			for iter.Next(&key, &val) {
				t.Logf("found %d: %d", key, val)
				count++
			}
			assert.Equal(t, 1, count, "one element in socket tracker map")
			last, err := s.LastActivity(uint16(port))
			if tc.expectLastActivity {
				assert.NoError(t, err)
				assert.Less(t, time.Since(last), time.Second)
			} else {
				assert.Error(t, err)
				assert.ErrorIs(t, err, NoActivityRecordedErr{})
			}
			assert.NoError(t, s.Reset())
			s.Stop(t.Context())
			bpf.Cleanup()
		})
	}
}

func startServer(t *testing.T, ctx context.Context, s *Server, port uint16, tc *testCase) {
	response := "ok"
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, response)
	}))
	if tc.connHook == nil {
		tc.connHook = func(c net.Conn) (net.Conn, bool, error) {
			return c, true, nil
		}
	}

	once := sync.Once{}
	err := s.Start(
		ctx,
		tc.connHook,
		func() error {
			once.Do(func() {
				// simulate a delay until our server is started
				time.Sleep(time.Millisecond * 200)
				network := "tcp4"
				if tc.ipv6 {
					network = "tcp6"
				}
				l, err := net.Listen(network, fmt.Sprintf(":%d", port))
				require.NoError(t, err)

				if err := s.DisableRedirects(); err != nil {
					t.Errorf("could not disable redirects: %s", err)
				}

				// replace listener of server
				ts.Listener.Close()
				ts.Listener = l
				ts.Start()
				t.Logf("listening on %s", l.Addr().String())

				t.Cleanup(func() {
					l.Close()
					ts.Close()
				})
			})
			return nil
		},
		port,
	)
	require.NoError(t, err)
	s.enableRedirect(port)
}
