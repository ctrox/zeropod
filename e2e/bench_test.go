package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func BenchmarkRestore(b *testing.B) {
	_, client, port := setup(b)
	ctx := context.Background()

	c := &http.Client{
		Timeout: time.Second * 10,
		Transport: &http.Transport{
			// disable keepalive as we want the container to checkpoint as soon as possible.
			DisableKeepAlives: true,
		},
	}

	benches := map[string]struct {
		preDump           bool
		scaleDownDuration time.Duration
	}{
		"without pre-dump": {
			preDump:           false,
			scaleDownDuration: 0,
		},
		"with pre-dump": {
			preDump:           true,
			scaleDownDuration: 0,
		},
	}

	waitDuration := time.Millisecond * 600

	for name, bc := range benches {
		bc := bc
		b.Run(name, func(b *testing.B) {
			// bench does an initial run with 1 iteration which we don't want since
			// the setup takes a long time.
			if b.N == 1 {
				b.ResetTimer()
				return
			}

			cleanupPod := createPodAndWait(b, ctx, client, testPod(bc.preDump, bc.scaleDownDuration))
			cleanupService := createServiceAndWait(b, ctx, client, testService(), 1)
			b.ResetTimer()

			defer func() {
				b.StopTimer()
				time.Sleep(waitDuration)
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
				time.Sleep(waitDuration)
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
