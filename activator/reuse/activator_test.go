package reuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
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
	cycles                 int
	expectLastActivity     bool
	trackerIgnoreLocalhost bool
	kubeletAddr            *netip.Addr
	networks               []Network
	forwardToFunc          func(t *testing.T, port int) (string, *httptest.Server)
}

func TestReuseActivator(t *testing.T) {
	if os.Getenv("IN_NET_PID_NS") == "1" {
		listenAndServe(t)
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
			cycles:             1,
			expectedBody:       "app",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
			networks:           []Network{NetworkTCP4},
		},
		"ipv6 only": {
			parallelReqs:       1,
			cycles:             1,
			expectedBody:       "app",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
			networks:           []Network{NetworkTCP6ONLY},
		},
		"100 in parallel": {
			parallelReqs:       100,
			cycles:             1,
			expectedBody:       "app",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
			networks:           []Network{NetworkTCPAny},
		},
		"ignore activity from localhost v4": {
			parallelReqs:           1,
			cycles:                 1,
			expectedBody:           "app",
			expectedCode:           http.StatusOK,
			expectLastActivity:     false,
			trackerIgnoreLocalhost: true,
			networks:               []Network{NetworkTCP4},
		},
		"ignore activity from localhost v6": {
			parallelReqs:           1,
			cycles:                 1,
			expectedBody:           "app",
			expectedCode:           http.StatusOK,
			expectLastActivity:     false,
			trackerIgnoreLocalhost: true,
			networks:               []Network{NetworkTCPAny},
		},
		"ignore kubelet traffic ipv4": {
			parallelReqs:       1,
			cycles:             1,
			expectedBody:       "ok\n",
			expectedCode:       http.StatusOK,
			expectLastActivity: false,
			kubeletAddr:        ptr.To(netip.MustParseAddr("127.0.0.1")),
			networks:           []Network{NetworkTCP4},
		},
		"ignore kubelet traffic ipv6": {
			parallelReqs:       1,
			cycles:             1,
			expectedBody:       "ok\n",
			expectedCode:       http.StatusOK,
			expectLastActivity: false,
			kubeletAddr:        ptr.To(netip.MustParseAddr("::1")),
			networks:           []Network{NetworkTCPAny},
		},
		"forward": {
			parallelReqs:       1,
			cycles:             1,
			expectedBody:       "hello from another server",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
			networks:           []Network{NetworkTCP4},
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
		"cycles": {
			parallelReqs:       1,
			cycles:             10,
			expectedBody:       "app",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
			networks:           []Network{NetworkTCPAny},
		},
		"ipv4 and ipv6": {
			parallelReqs:       1,
			cycles:             1,
			expectedBody:       "app",
			expectedCode:       http.StatusOK,
			expectLastActivity: true,
			networks:           []Network{NetworkTCPAny},
		},
	}
	wg := sync.WaitGroup{}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			port, err := freePort()
			require.NoError(t, err)
			require.NoError(t, log.SetLevel(log.DebugLevel.String()))
			ctx, cancel := context.WithCancel(t.Context())

			// TODO: figure out why forwardToFunc leaks fds (maybe also the forwarder itself)
			if tc.forwardToFunc == nil {
				defer checkFDLeaks(t)()
			}
			s, err := New(ctx, nn, "/sys/fs/cgroup")
			require.NoError(t, err)

			cmd, err := runApp(t, port, tc.networks...)
			require.NoError(t, err)
			fmt.Printf("app pid %d\n", cmd.Process.Pid)

			require.NoError(t, s.Reload(
				ProbeAddr(tc.kubeletAddr),
				TrackerIgnoreLocalhost(tc.trackerIgnoreLocalhost),
				// TODO: not sure why but the socket migration breaks when we
				// run the app in the hook itself
				RestoreHook(func() (int, error) {
					time.Sleep(time.Millisecond * 10)
					return cmd.Process.Pid, nil
				}),
			))

			t.Cleanup(func() {
				cancel()
			})

			listeners := Listeners{}
			for _, net := range tc.networks {
				listeners = append(listeners, Listener{Port: uint16(port), Network: net})
			}
			require.NoError(t, s.Start(ctx, os.Getpid(), listeners, true))
			if tc.forwardToFunc != nil {
				addr, ts := tc.forwardToFunc(t, port)
				defer ts.Close()
				assert.NoError(t, s.ForwardToTarget(ctx, addr))
				assert.NoError(t, s.Reload(RestoreHook(func() (int, error) { return 0, nil })))
			}

			for range tc.cycles {
				for i := 0; i < tc.parallelReqs; i++ {
					for _, net := range tc.networks {
						wg.Go(func() {
							host := "127.0.0.1"
							if net == NetworkTCP6ONLY || net == NetworkTCPAny {
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
				}
				wg.Wait()
				assert.NoError(t, s.ScaleDown())
				for _, ln := range s.listeners {
					if err := ln.reuse.Listeners.Delete(uint32(appKey)); err != nil {
						if !errors.Is(err, ebpf.ErrKeyNotExist) {
							assert.NoError(t, err)
						}
					}
				}
			}
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
			assert.NoError(t, s.Stop())
			assert.NoError(t, cmd.Process.Kill())
			_ = cmd.Wait()
		})
	}
}

func runApp(t *testing.T, port int, networks ...Network) (*exec.Cmd, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestReuseActivator$")

	nets := []string{}
	for _, net := range networks {
		nets = append(nets, string(net))
	}
	cmd.Env = append(
		os.Environ(),
		"IN_NET_PID_NS=1",
		fmt.Sprintf("NETWORKS=%s", strings.Join(nets, ",")),
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
	w.Close()
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
	t.Logf("using pid %d", cmd.Process.Pid)
	return cmd, nil
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

func listenAndServe(t *testing.T) {
	wg := sync.WaitGroup{}
	networks := strings.SplitSeq(os.Getenv("NETWORKS"), ",")
	fd := uintptr(3)
	for n := range networks {
		fmt.Printf("listening on %s\n", n)
		ln, err := net.Listen(n, os.Getenv("ADDRESS"))
		if err != nil {
			t.Fatalf("create listener in isolated netns: %v", err)
		}
		defer ln.Close()
		pipe := os.NewFile(fd, "pipe")
		if pipe != nil {
			pipe.Write([]byte{1})
			pipe.Close()
		}
		fd++
		wg.Go(func() {
			http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, os.Getenv("RESPONSE"))
			}))
		})
	}
	fmt.Println("serving")
	wg.Wait()
	fmt.Println("wait done")
}

func checkFDLeaks(t *testing.T) func() {
	t.Helper()
	before := getFDs(t)

	return func() {
		t.Helper()

		after := map[string]string{}
		// some fds can take a bit of time to release so we retry a bunch
		for range 10 {
			after = getFDs(t)
			if len(after) <= len(before) {
				break
			}
			time.Sleep(time.Millisecond * 100)
		}
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
