package e2e

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ctrox/zeropod/zeropod"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

const runtimeClassName = "zeropod"

func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test")
	}

	cfg, client, port := setup(t)
	ctx := context.Background()

	c := &http.Client{
		Timeout: time.Second * 10,
	}

	cases := map[string]struct {
		pod            *corev1.Pod
		svc            *corev1.Service
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
			pod:            testPod(scaleDownAfter(0)),
			parallelReqs:   1,
			sequentialReqs: 1,
			preDump:        false,
			maxReqDuration: time.Second,
		},
		"with pre-dump": {
			pod:            testPod(preDump(true), scaleDownAfter(0)),
			parallelReqs:   1,
			sequentialReqs: 1,
			preDump:        true,
			maxReqDuration: time.Second,
		},
		// this is a blackbox test for the socket tracking. We know that
		// restoring a snapshot of the test pod takes at least 50ms, even on
		// very fast systems. So if a request takes longer than that we know
		// that the socket tracking does not work as it means the container
		// has checkpointed.
		"socket tracking": {
			pod:            testPod(scaleDownAfter(time.Second)),
			parallelReqs:   1,
			sequentialReqs: 20,
			keepAlive:      false,
			sequentialWait: time.Millisecond * 200,
			maxReqDuration: time.Millisecond * 50,
			ignoreFirstReq: true,
		},
		"pod without configuration": {
			pod:            testPod(),
			parallelReqs:   1,
			sequentialReqs: 1,
			maxReqDuration: time.Second,
		},
		"pod with multiple containers": {
			pod:            testPod(agnContainer("c1", 8080), agnContainer("c2", 8081), scaleDownAfter(0)),
			svc:            testService(8081),
			parallelReqs:   1,
			sequentialReqs: 1,
			maxReqDuration: time.Second,
		},
		"pod selecting specific containers": {
			pod: testPod(
				agnContainer("c1", 8080),
				agnContainer("c2", 8081),
				agnContainer("c3", 8082),
				containerNamesAnnotation("c1,c3"),
				portsAnnotation("c1=8080;c3=8082"),
			),
			svc:            testService(8082),
			parallelReqs:   1,
			sequentialReqs: 1,
			maxReqDuration: time.Second,
		},

		// TODO: fix parallel tests. These are flaky at the moment, probably
		// because the actual activator implementation is buggy.
		// "parallel requests": {
		// 	pod:            testPod(false, 0),
		// 	parallelReqs:   4,
		// 	sequentialReqs: 1,
		// 	maxReqDuration: time.Second * 2,
		// },
		// "parallel requests with keepalive": {
		// 	pod:            testPod(false, 0),
		// 	parallelReqs:   4,
		// 	sequentialReqs: 1,
		// 	keepAlive:      true,
		// 	maxReqDuration: time.Second * 2,
		// },
	}

	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			if tc.preDump && runtime.GOARCH == "arm64" {
				t.Skip("skipping pre-dump test as it's not supported on arm64")
			}

			if tc.svc == nil {
				tc.svc = testService(defaultTargetPort)
			}

			cleanupPod := createPodAndWait(t, ctx, client, tc.pod)
			cleanupService := createServiceAndWait(t, ctx, client, tc.svc, 1)
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

	t.Run("exec", func(t *testing.T) {
		pod := testPod(scaleDownAfter(0))
		cleanupPod := createPodAndWait(t, ctx, client, pod)
		defer cleanupPod()

		stdout, stderr, err := podExec(cfg, pod, "date")
		require.NoError(t, err)
		t.Log(stdout, stderr)

		// as we can't yet reliably check if the pod is fully checkpointed and
		// ready for another exec, we simply retry
		require.Eventually(t, func() bool {
			stdout, stderr, err = podExec(cfg, pod, "date")
			t.Log(stdout, stderr)
			return err == nil
		}, time.Second*10, time.Second)

		assert.GreaterOrEqual(t, restoreCount(t, client, cfg, pod), 2, "pod should have been restored 2 times")
	})

	t.Run("metrics", func(t *testing.T) {
		// create two pods to test metric merging
		runningPod := testPod(scaleDownAfter(time.Hour))
		cleanupRunningPod := createPodAndWait(t, ctx, client, runningPod)
		defer cleanupRunningPod()

		checkpointedPod := testPod(scaleDownAfter(0))
		cleanupCheckpointedPod := createPodAndWait(t, ctx, client, checkpointedPod)
		defer cleanupCheckpointedPod()

		restoredPod := testPod(scaleDownAfter(0))
		cleanupRestoredPod := createPodAndWait(t, ctx, client, restoredPod)
		defer cleanupRestoredPod()

		// exec into pod to ensure it has been restored at least once
		require.Eventually(t, func() bool {
			_, _, err := podExec(cfg, restoredPod, "date")
			return err == nil
		}, time.Second*10, time.Second)

		mfs := getNodeMetrics(t, client, cfg)

		tests := map[string]struct {
			metric               string
			pod                  *corev1.Pod
			gaugeValue           *float64
			histogramSampleCount *uint64
		}{
			"running": {
				metric:     prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricRunning),
				gaugeValue: ptr.To(float64(1)),
				pod:        runningPod,
			},
			"not running": {
				metric:     prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricRunning),
				gaugeValue: ptr.To(float64(0)),
				pod:        checkpointedPod,
			},
			"last checkpoint time": {
				metric: prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricLastCheckpointTime),
				pod:    checkpointedPod,
			},
			"checkpoint duration": {
				metric: prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricCheckPointDuration),
				pod:    checkpointedPod,
				// we expect two checkpoints as the first one happens due to the startupProbe
				histogramSampleCount: ptr.To(uint64(2)),
			},
			"last restore time": {
				metric: prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricLastRestoreTime),
				pod:    restoredPod,
			},
			"restore duration": {
				metric: prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricRestoreDuration),
				pod:    restoredPod,
				// we expect two restores as the first one happens due to the startupProbe
				histogramSampleCount: ptr.To(uint64(2)),
			},
		}

		for name, tc := range tests {
			tc := tc
			t.Run(name, func(t *testing.T) {
				val, ok := mfs[tc.metric]
				if !ok {
					t.Fatalf("could not find expected metric: %s", tc.metric)
				}

				metric, ok := findMetricByLabelMatch(val.Metric, map[string]string{
					zeropod.LabelPodName:      tc.pod.Name,
					zeropod.LabelPodNamespace: tc.pod.Namespace,
				})
				if !ok {
					t.Fatalf("could not find expected metric for pod: %s/%s", tc.pod.Name, tc.pod.Namespace)
				}

				if tc.gaugeValue != nil {
					assert.Equal(t, *tc.gaugeValue, *metric.Gauge.Value,
						"gauge value does not match expectation")
				}

				if tc.histogramSampleCount != nil {
					assert.Equal(t, *tc.histogramSampleCount, *metric.Histogram.SampleCount,
						"histogram sample count does not match expectation")
				}
			})
		}
	})
}
