package socket

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ctrox/zeropod/activator"
	"github.com/stretchr/testify/require"
)

// TestEBPFTracker tests the ebpf tcp tracker by getting our own pid, starting
// an HTTP server and doing a request against it. This test requires elevated
// privileges to run.
func TestEBPFTracker(t *testing.T) {
	require.NoError(t, activator.MountDebugFS())
	require.NoError(t, activator.MountBPFFS(activator.BPFFSPath))

	clean, err := LoadEBPFTracker()
	require.NoError(t, err)
	defer func() { require.NoError(t, clean()) }()

	tracker, err := NewEBPFTracker()
	require.NoError(t, err)

	pid := uint32(os.Getpid())
	require.NoError(t, tracker.TrackPid(pid))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	}))

	require.Eventually(t, func() bool {
		_, err = http.Get(ts.URL)
		return err == nil
	}, time.Millisecond*100, time.Millisecond, "waiting for http server to reply")

	require.Eventually(t, func() bool {
		activity, err := tracker.LastActivity(pid)
		if err != nil {
			return false
		}

		if time.Since(activity) > time.Millisecond*100 {
			t.Fatalf("last activity was %s ago, expected it to be within the last 100ms", time.Since(activity))
		}

		return true
	}, time.Millisecond*100, time.Millisecond, "waiting for last tcp activity")
}
