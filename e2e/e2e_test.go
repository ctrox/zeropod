package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

const runtimeClassName = "zeropod"

func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test")
	}

	_, client, port := setup(t)
	ctx := context.Background()

	c := &http.Client{
		Timeout: time.Second * 10,
	}

	cases := map[string]struct {
		pod              *corev1.Pod
		parallelRequests int
		keepAlive        bool
	}{
		"without pre-dump": {
			pod:              testPod(false, 0),
			parallelRequests: 1,
		},
		"parallel requests": {
			pod:              testPod(false, 0),
			parallelRequests: 4,
		},
		"parallel requests with keepalive": {
			pod:              testPod(false, 0),
			parallelRequests: 4,
			keepAlive:        true,
		},
	}

	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			svc := testService()

			cleanupPod := createPodAndWait(t, ctx, client, tc.pod)
			cleanupService := createServiceAndWait(t, ctx, client, svc, 1)
			defer cleanupPod()
			defer cleanupService()

			wg := sync.WaitGroup{}
			wg.Add(tc.parallelRequests)
			for i := 0; i < tc.parallelRequests; i++ {
				go func() {
					defer wg.Done()

					c.Transport = &http.Transport{DisableKeepAlives: !tc.keepAlive}

					before := time.Now()
					resp, err := c.Get(fmt.Sprintf("http://localhost:%d", port))
					if err != nil {
						t.Error(err)
						return
					}
					t.Logf("request took %s", time.Since(before))
					assert.Equal(t, resp.StatusCode, http.StatusOK)
				}()
			}
			wg.Wait()
		})
	}
}
