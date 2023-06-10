package e2e

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
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
		pod            *corev1.Pod
		parallelReqs   int
		sequentialReqs int
		sequentialWait time.Duration
		maxReqDuration time.Duration
		ignoreFirstReq bool
		keepAlive      bool
		preDump        bool
	}{
		// note: some of these max request durations are really
		// system-dependent. It has been tested on a few systems so far and
		// they should leave enough headroom but the tests could be flaky
		// because of that.
		"without pre-dump": {
			pod:            testPod(false, 0),
			parallelReqs:   1,
			sequentialReqs: 1,
			preDump:        false,
			maxReqDuration: time.Second,
		},
		"with pre-dump": {
			pod:            testPod(true, 0),
			parallelReqs:   1,
			sequentialReqs: 1,
			preDump:        true,
			maxReqDuration: time.Second,
		},
		"parallel requests": {
			pod:            testPod(false, 0),
			parallelReqs:   4,
			sequentialReqs: 1,
			maxReqDuration: time.Second * 2,
		},
		"parallel requests with keepalive": {
			pod:            testPod(false, 0),
			parallelReqs:   4,
			sequentialReqs: 1,
			keepAlive:      true,
			maxReqDuration: time.Second * 2,
		},
		// this is a blackbox test for the socket tracking. We know that
		// restoring a snapshot of the test pod takes at least 50ms, even on
		// very fast systems. So if a request takes longer than that we know
		// that the socket tracking does not work as it means the container
		// has checkpointed.
		"socket tracking": {
			pod:            testPod(false, time.Second),
			parallelReqs:   1,
			sequentialReqs: 20,
			keepAlive:      false,
			sequentialWait: time.Millisecond * 200,
			maxReqDuration: time.Millisecond * 50,
			ignoreFirstReq: true,
		},
	}

	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			if tc.preDump && runtime.GOARCH == "arm64" {
				t.Skip("skipping pre-dump test as it's not supported on arm64")
			}

			svc := testService()

			cleanupPod := createPodAndWait(t, ctx, client, tc.pod)
			cleanupService := createServiceAndWait(t, ctx, client, svc, 1)
			defer cleanupPod()
			defer cleanupService()

			wg := sync.WaitGroup{}
			wg.Add(tc.parallelReqs)
			for i := 0; i < tc.parallelReqs; i++ {
				go func() {
					defer wg.Done()
					for x := 0; x < tc.sequentialReqs; x++ {
						defer time.Sleep(tc.sequentialWait)
						c.Transport = &http.Transport{DisableKeepAlives: !tc.keepAlive}

						before := time.Now()
						resp, err := c.Get(fmt.Sprintf("http://localhost:%d", port))
						if err != nil {
							t.Error(err)
							return
						}
						t.Logf("request took %s", time.Since(before))
						assert.Equal(t, resp.StatusCode, http.StatusOK)
						if tc.ignoreFirstReq && x == 0 {
							continue
						}
						assert.Less(t, time.Since(before), tc.maxReqDuration)
					}
				}()
			}
			wg.Wait()
		})
	}
}
