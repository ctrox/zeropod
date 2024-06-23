package e2e

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ctrox/zeropod/manager"
	"github.com/ctrox/zeropod/zeropod"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
		"parallel requests": {
			pod:            testPod(scaleDownAfter(time.Second)),
			parallelReqs:   10,
			sequentialReqs: 5,
			maxReqDuration: time.Second,
		},
		"parallel requests with keepalive": {
			pod:            testPod(scaleDownAfter(time.Second)),
			parallelReqs:   10,
			sequentialReqs: 5,
			keepAlive:      true,
			maxReqDuration: time.Second,
		},
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

		require.Eventually(t, func() bool {
			checkpointed, err := isCheckpointed(t, client, cfg, pod)
			if err != nil {
				t.Logf("error checking if checkpointed: %s", err)
				return false
			}
			return checkpointed
		}, time.Minute, time.Second)

		stdout, stderr, err := podExec(cfg, pod, "date")
		require.NoError(t, err)
		t.Log(stdout, stderr)

		require.Eventually(t, func() bool {
			count, err := restoreCount(t, client, cfg, pod)
			if err != nil {
				t.Logf("error checking if restored: %s", err)
				return false
			}
			return assert.GreaterOrEqual(t, count, 1, "pod should have been restored at least once")
		}, time.Minute, time.Second)
	})

	t.Run("delete in restored state", func(t *testing.T) {
		// as we want to delete the pod when it is in a restored state, we
		// first need to make sure it has checkpointed at least once.
		pod := testPod(scaleDownAfter(0))
		cleanupPod := createPodAndWait(t, ctx, client, pod)
		defer cleanupPod()

		require.Eventually(t, func() bool {
			checkpointed, err := isCheckpointed(t, client, cfg, pod)
			if err != nil {
				t.Logf("error checking if checkpointed: %s", err)
				return false
			}
			return checkpointed
		}, time.Minute, time.Second)

		stdout, stderr, err := podExec(cfg, pod, "date")
		require.NoError(t, err)
		t.Log(stdout, stderr)
		// since the cleanup has been deferred it's called right after the
		// exec and should test the deletion in the restored state.
	})

	t.Run("resources scaling", func(t *testing.T) {
		pod := testPod(scaleDownAfter(0), agnContainer("agn", 8080), resources(corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			},
		}))

		cleanupPod := createPodAndWait(t, ctx, client, pod)
		defer cleanupPod()
		require.Eventually(t, func() bool {
			if err := client.Get(ctx, objectName(pod), pod); err != nil {
				return false
			}

			resourcesScaledDown := false
			for _, container := range pod.Status.ContainerStatuses {
				t.Logf("allocated resources: %v", container.AllocatedResources)
				resourcesScaledDown = container.AllocatedResources != nil &&
					container.AllocatedResources[corev1.ResourceCPU] == manager.ScaledDownCPU &&
					container.AllocatedResources[corev1.ResourceMemory] == manager.ScaledDownMemory
			}

			return resourcesScaledDown
		}, time.Minute, time.Second)
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
			if err != nil {
				t.Logf("error during pod exec: %s", err)
				return false
			}
			checkpointed, err := isCheckpointed(t, client, cfg, restoredPod)
			if err != nil {
				t.Logf("error checking if checkpointed: %s", err)
				return false
			}
			return checkpointed
		}, time.Minute, time.Second)

		mfs := getNodeMetrics(t, client, cfg)

		tests := map[string]struct {
			metric                  string
			pod                     *corev1.Pod
			gaugeValue              *float64
			minHistogramSampleCount *uint64
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
				metric:                  prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricCheckPointDuration),
				pod:                     checkpointedPod,
				minHistogramSampleCount: ptr.To(uint64(1)),
			},
			"last restore time": {
				metric: prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricLastRestoreTime),
				pod:    restoredPod,
			},
			"restore duration": {
				metric:                  prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricRestoreDuration),
				pod:                     restoredPod,
				minHistogramSampleCount: ptr.To(uint64(1)),
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

				if tc.minHistogramSampleCount != nil {
					assert.GreaterOrEqual(t, *metric.Histogram.SampleCount, *tc.minHistogramSampleCount,
						"histogram sample count does not match expectation")
				}
			})
		}
	})
}
