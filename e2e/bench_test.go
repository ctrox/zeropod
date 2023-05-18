package e2e

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func BenchmarkRestore(b *testing.B) {
	// bench does an initial run with 1 iteration which we don't want since
	// the setup takes a long time.
	if b.N == 1 {
		b.ResetTimer()
		return
	}

	_, client, port := setup(b)
	ctx := context.Background()

	pod := testPod()
	svc := testService()
	createPodAndWait(b, ctx, client, pod)
	createServiceAndWait(b, ctx, client, svc, 1)

	c := &http.Client{
		Transport: &http.Transport{
			// disable keepalive as we want the container to checkpoint as soon as possible.
			DisableKeepAlives: true,
		},
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// we add a sleep in between requests so we can be sure the container
		// has checkpointed completely when we hit it again.
		time.Sleep(time.Millisecond * 300)
		b.StartTimer()

		resp, err := c.Get(fmt.Sprintf("http://localhost:%d", port))
		require.NoError(b, err)
		assert.Equal(b, resp.StatusCode, http.StatusOK)
	}
}
