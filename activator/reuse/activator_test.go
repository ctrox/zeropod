package reuse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/activator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
)

type testCase struct {
	parallelReqs           int
	expectedBody           string
	expectedCode           int
	expectLastActivity     bool
	ipv6                   bool
	trackerIgnoreLocalhost bool
	kubeletAddr            *netip.Addr
	forwardToFunc          func(t *testing.T, port int) (string, *httptest.Server)
}

func TestReuseActivator(t *testing.T) {
	if os.Getenv("IN_NET_PID_NS") == "1" {
		listen(t)
		return
	}

	require.NoError(t, activator.MountBPFFS(activator.BPFFSPath))
	nn, err := ns.GetCurrentNS()
	require.NoError(t, err)

	c := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	tests := map[string]testCase{
		"ipv4": {
			parallelReqs:       1,
			expectedBody:       "app",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
		},
		"ipv6": {
			parallelReqs:       1,
			expectedBody:       "app",
			expectedCode:       http.StatusOK,
			ipv6:               true,
			expectLastActivity: true,
		},
		"100 in parallel": {
			parallelReqs:       100,
			expectedBody:       "app",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
		},
		"ignore activity from localhost v4": {
			parallelReqs:           1,
			expectedBody:           "app",
			expectedCode:           http.StatusOK,
			ipv6:                   false,
			expectLastActivity:     false,
			trackerIgnoreLocalhost: true,
		},
		"ignore activity from localhost v6": {
			parallelReqs:           1,
			expectedBody:           "app",
			expectedCode:           http.StatusOK,
			ipv6:                   true,
			expectLastActivity:     false,
			trackerIgnoreLocalhost: true,
		},
		"ignore kubelet traffic ipv4": {
			parallelReqs:       1,
			expectedBody:       "ok\n",
			expectedCode:       http.StatusOK,
			ipv6:               false,
			expectLastActivity: false,
			kubeletAddr:        ptr.To(netip.MustParseAddr("127.0.0.1")),
		},
		"ignore kubelet traffic ipv6": {
			parallelReqs:       1,
			expectedBody:       "ok\n",
			expectedCode:       http.StatusOK,
			ipv6:               true,
			expectLastActivity: false,
			kubeletAddr:        ptr.To(netip.MustParseAddr("::1")),
		},
		"forward": {
			parallelReqs:       1,
			expectedBody:       "hello from another server",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
			forwardToFunc: func(t *testing.T, port int) (string, *httptest.Server) {
				ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, "hello from another server")
				}))
				// usually forwarding happens to a different pod IP, to simulate
				// that we just use a different localhost IP
				l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.2:%d", port))
				if err != nil {
					t.Fatal(err)
				}
				ts.Listener.Close()
				ts.Listener = l
				ts.Start()
				return "127.0.0.2", ts
			},
		},
	}
	wg := sync.WaitGroup{}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			defer checkFDLeaks(t)()
			port, err := freePort()
			require.NoError(t, err)
			once := &sync.Once{}
			require.NoError(t, log.SetLevel(log.DebugLevel.String()))
			ctx, cancel := context.WithCancel(t.Context())

			s, err := New(
				ctx, nn, "/sys/fs/cgroup",
			)
			require.NoError(t, err)

			pid, err := runApp(t, tc, once, port)
			require.NoError(t, err)

			require.NoError(t, s.Reload(
				ProbeAddr(tc.kubeletAddr),
				TrackerIgnoreLocalhost(tc.trackerIgnoreLocalhost),
				// TODO: not sure why but the socket migration breaks when we
				// run the app in the hook itself
				RestoreHook(func() (int, error) { return pid, nil }),
			))

			t.Cleanup(func() {
				cancel()
			})

			network := NetworkTCP4
			if tc.ipv6 {
				network = NetworkTCP6ONLY
			}
			require.NoError(t, s.Start(ctx, os.Getpid(), Listeners{{Port: uint16(port), Network: network}}, true))
			if tc.forwardToFunc != nil {
				addr, ts := tc.forwardToFunc(t, port)
				defer ts.Close()
				assert.NoError(t, s.ForwardToTarget(ctx, addr))
				assert.NoError(t, s.Reload(RestoreHook(func() (int, error) { return 0, nil })))
			}

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
			var key uint32
			var val uint64
			count := 0
			iter := s.trackerObjs.SocketTracker.Iterate()
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
				assert.ErrorIs(t, err, activator.NoActivityRecordedErr{})
			}
			cancel()
			s.Stop()
		})
	}
}

func runApp(t *testing.T, tc testCase, once *sync.Once, port int) (int, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestReuseActivator$")
	once.Do(func() {
		network := "tcp4"
		if tc.ipv6 {
			network = "tcp6"
		}

		cmd.Env = append(
			os.Environ(),
			"IN_NET_PID_NS=1",
			fmt.Sprintf("NETWORK=%s", network),
			fmt.Sprintf("ADDRESS=:%d", port),
			fmt.Sprintf("PORT=%d", port),
			fmt.Sprintf("RESPONSE=%s", "app"),
			"GODEBUG=multipathtcp=0",
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		cmd.SysProcAttr = &syscall.SysProcAttr{
			Cloneflags: syscall.CLONE_NEWPID,
		}
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("failed to create pipe: %v", err)
		}
		defer r.Close()
		cmd.ExtraFiles = []*os.File{w}

		require.NoError(t, cmd.Start())
		t.Cleanup(func() {
			cmd.Process.Kill()
			cmd.Wait()
		})
		ready := make(chan struct{})
		go func() {
			buf := make([]byte, 1)
			r.Read(buf)
			close(ready)
		}()
		<-ready
	})
	t.Logf("using pid %d", cmd.Process.Pid)
	return cmd.Process.Pid, nil
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, err
	}

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("addr is not a net.TCPAddr: %T", listener.Addr())
	}

	if err := listener.Close(); err != nil {
		return 0, err
	}

	return addr.Port, nil
}

func listen(t *testing.T) {
	ln, err := net.Listen(os.Getenv("NETWORK"), os.Getenv("ADDRESS"))
	if err != nil {
		t.Fatalf("create listener in isolated netns: %v", err)
	}
	defer ln.Close()
	fmt.Printf("listening on %s %s inside PID %d\n", ln.Addr(), os.Getenv("NETWORK"), os.Getpid())
	pipe := os.NewFile(3, "pipe")
	if pipe != nil {
		pipe.Write([]byte{1})
		pipe.Close()
	}
	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, os.Getenv("RESPONSE"))
	}))
}

func checkFDLeaks(t *testing.T) func() {
	t.Helper()
	before := getFDs(t)

	return func() {
		t.Helper()
		after := getFDs(t)

		if len(after) > len(before) {
			b, err := json.MarshalIndent(diff(before, after), "", "  ")
			assert.NoError(t, err)
			t.Errorf("file descriptor leak detected! Before: %d, After: %d\nLeaked FDs: %s",
				len(before), len(after), b)
		}
	}
}

func getFDs(t *testing.T) map[string]string {
	t.Helper()

	fdDir := "/proc/self/fd"
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatalf("failed to read open FDs: %v", err)
	}

	fds := make(map[string]string, len(entries))
	for _, entry := range entries {
		target, err := os.Readlink(fdDir + "/" + entry.Name())
		if err != nil {
			target = "unknown"
		}
		fds[entry.Name()] = target
	}
	return fds
}

func diff(before, after map[string]string) map[string]string {
	leaked := make(map[string]string)
	for fd, target := range after {
		if _, ok := before[fd]; !ok {
			leaked[fd] = target
		}
	}
	return leaked
}
