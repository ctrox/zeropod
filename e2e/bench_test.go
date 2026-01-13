package e2e

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func BenchmarkRestore(b *testing.B) {
	e2e := setup(b)
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
		waitDuration   time.Duration
		scaleDownAfter time.Duration
	}{
		"without pre-dump": {
			preDump:        false,
			scaleDownAfter: 0,
			waitDuration:   time.Millisecond * 800,
		},
		"with pre-dump": {
			preDump:        true,
			scaleDownAfter: 0,
			waitDuration:   time.Millisecond * 800,
		},
		"one second scaledown duration": {
			preDump:        false,
			scaleDownAfter: time.Second,
			waitDuration:   0,
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

			cleanupPod := createPodAndWait(b, ctx, client, testPod(preDump(bc.preDump), scaleDownAfter(bc.scaleDownAfter)))
			cleanupService := createServiceAndWait(b, ctx, client, testService(defaultTargetPort), 1)
			b.ResetTimer()

			defer func() {
				b.StopTimer()
				time.Sleep(bc.waitDuration)
				cleanupPod()
				cleanupService()
				b.StartTimer()
			}()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// we add a sleep in between requests so we can be sure the
				// container has checkpointed completely when we hit it again.
				// TODO: once we are able to tell if the container has
				// checkpointed from the outside, we should just wait for that
				// instead of the static sleep.
				time.Sleep(bc.waitDuration)
				b.StartTimer()

				before := time.Now()
				resp, err := c.Get(fmt.Sprintf("http://localhost:%d", port))
				if !assert.NoError(b, err) {
					b.Log("error", err)
					time.Sleep(time.Hour)
					continue
				}
				b.Logf("get request took %s", time.Since(before))
				assert.Equal(b, resp.StatusCode, http.StatusOK)
			}
		})
	}
}
