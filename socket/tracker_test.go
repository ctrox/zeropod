package socket

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ctrox/zeropod/activator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEBPFTracker tests the ebpf tcp tracker by getting our own pid, starting
// an HTTP server and doing a request against it. This test requires elevated
// privileges to run.
func TestEBPFTracker(t *testing.T) {
	require.NoError(t, activator.MountBPFFS(activator.BPFFSPath))

	name, err := os.Executable()
	require.NoError(t, err)
	tracker, clean, err := LoadEBPFTracker(filepath.Base(name))
	require.NoError(t, err)
	defer func() { require.NoError(t, clean()) }()

	pid := uint32(os.Getpid())
	require.NoError(t, tracker.TrackPid(pid))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))

	for name, tc := range map[string]struct {
		ip                 netip.Addr
		expectLastActivity bool
	}{
		"activity tracked": {
			ip:                 netip.MustParseAddr("10.0.0.1"),
			expectLastActivity: true,
		},
		"activity ignored": {
			// use 127.0.0.1 as that's where our test program connects from
			ip:                 netip.MustParseAddr("127.0.0.1"),
			expectLastActivity: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, tracker.PutPodIP(tc.ip))
			defer func() { assert.NoError(t, tracker.RemovePodIP(tc.ip)) }()

			require.Eventually(t, func() bool {
				_, err = http.Get(ts.URL)
				return err == nil
			}, time.Millisecond*100, time.Millisecond, "waiting for http server to reply")

			require.Eventually(t, func() bool {
				activity, err := tracker.LastActivity(pid)
				if err != nil {
					return !tc.expectLastActivity
				}

				if time.Since(activity) > time.Millisecond*100 {
					if tc.expectLastActivity {
						t.Fatalf("last activity was %s ago, expected it to be within the last 100ms", time.Since(activity))
					}
				}

				return true
			}, time.Millisecond*100, time.Millisecond, "waiting for last tcp activity")
			time.Sleep(time.Millisecond * 200)
		})
	}
}
