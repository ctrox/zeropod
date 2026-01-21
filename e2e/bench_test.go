package e2e

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func BenchmarkRestore(b *testing.B) {
	e2e := setup(b)
	defer e2e.cleanup()
	client := e2e.client
	port := e2e.port
	ctx := context.Background()

	c := &http.Client{
		Timeout: time.Second * 10,
		Transport: &http.Transport{
			// disable keepalive as we want the container to checkpoint as soon as possible.
			DisableKeepAlives: true,
		},
	}

	benches := map[string]struct {
		preDump        bool
		imageStreaming bool
		scaleDownAfter time.Duration
	}{
		"without pre-dump": {
			preDump:        false,
			scaleDownAfter: 0,
		},
		"with pre-dump": {
			preDump:        true,
			scaleDownAfter: 0,
		},
		"one second scaledown duration": {
			preDump:        false,
			scaleDownAfter: time.Second,
		},
		"without image streaming": {
			imageStreaming: false,
			scaleDownAfter: time.Millisecond * 200,
		},
		"with image streaming": {
			imageStreaming: true,
			scaleDownAfter: time.Millisecond * 200,
		},
	}

	for name, bc := range benches {
		b.Run(name, func(b *testing.B) {
			if bc.preDump && runtime.GOARCH == "arm64" {
				b.Skip("skipping pre-dump test as it's not supported on arm64")
			}

			// bench does an initial run with 1 iteration which we don't want since
			// the setup takes a long time.
			if b.N == 1 {
				b.ResetTimer()
				return
			}
			pod := testPod(addContainer("freezer", "ghcr.io/ctrox/zeropod-freezer", []string{"-memory", strconv.Itoa(128)}, 8080), preDump(bc.preDump), scaleDownAfter(bc.scaleDownAfter), imageStreaming(bc.imageStreaming))
			cleanupPod := createPodAndWait(b, ctx, client, pod)
			cleanupService := createServiceAndWait(b, ctx, client, testService(8080), 1)
			b.ResetTimer()

			defer func() {
				b.StopTimer()
				waitUntilScaledDown(b, ctx, e2e.client, pod)
				cleanupPod()
				cleanupService()
				b.StartTimer()
			}()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				waitUntilScaledDown(b, ctx, e2e.client, pod)
				b.StartTimer()

				before := time.Now()
				resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/get", port))
				if !assert.NoError(b, err) {
					b.Log("error", err)
					time.Sleep(time.Hour)
					continue
				}
				b.Logf("get request took %s", time.Since(before))
				assert.Equal(b, resp.StatusCode, http.StatusOK)
				b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/op")
				b.ReportMetric(0, "ns/op")
			}
		})
	}
}
