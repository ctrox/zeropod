package e2e

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/manager"
	"github.com/ctrox/zeropod/shim"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test")
	}
	e2e := setupOnce(t)
	ctx := context.Background()

	c := &http.Client{
		Timeout: time.Second * 10,
	}
	defaultScaleDownAfter := scaleDownAfter(time.Second)

	cases := map[string]struct {
		pod              *corev1.Pod
		svc              *corev1.Service
		parallelReqs     int
		sequentialReqs   int
		sequentialWait   time.Duration
		maxReqDuration   time.Duration
		ignoreFirstReq   bool
		keepAlive        bool
		preDump          bool
		waitScaledDown   bool
		expectRunning    bool
		expectScaledDown bool
	}{
		// note: some of these max request durations are really
		// system-dependent. It has been tested on a few systems so far and
		// they should leave enough headroom but the tests could be flaky
		// because of that.
		"without pre-dump": {
			pod:            testPod(defaultScaleDownAfter),
			parallelReqs:   1,
			sequentialReqs: 1,
			preDump:        false,
			maxReqDuration: time.Second,
			waitScaledDown: true,
		},
		"with pre-dump": {
			pod:            testPod(preDump(true), defaultScaleDownAfter),
			parallelReqs:   1,
			sequentialReqs: 1,
			preDump:        true,
			maxReqDuration: time.Second,
			waitScaledDown: true,
		},
		// this is a blackbox test for the socket tracking. We know that
		// restoring a snapshot of the test pod takes at least 50ms, even on
		// very fast systems. So if a request takes longer than that we know
		// that the socket tracking does not work as it means the container
		// has checkpointed.
		"socket tracking": {
			pod:            testPod(defaultScaleDownAfter),
			parallelReqs:   1,
			sequentialReqs: 20,
			keepAlive:      false,
			sequentialWait: time.Millisecond * 200,
			maxReqDuration: time.Millisecond * 50,
			ignoreFirstReq: true,
			waitScaledDown: true,
		},
		"pod without configuration": {
			pod:            testPod(),
			parallelReqs:   1,
			sequentialReqs: 1,
			maxReqDuration: time.Second,
			expectRunning:  true,
		},
		"pod with scaledown disabled": {
			pod:            testPod(scaleDownAfter(0)),
			parallelReqs:   1,
			sequentialReqs: 1,
			maxReqDuration: time.Second,
			expectRunning:  true,
		},
		"pod with multiple containers": {
			pod:            testPod(agnContainer("c1", 8080), agnContainer("c2", 8081), defaultScaleDownAfter),
			svc:            testService(8081),
			parallelReqs:   1,
			sequentialReqs: 1,
			maxReqDuration: time.Second,
			waitScaledDown: true,
		},
		"pod selecting specific containers": {
			pod: testPod(
				agnContainer("c1", 8080),
				agnContainer("c2", 8081),
				agnContainer("c3", 8082),
				containerNamesAnnotation("c1,c3"),
				portsAnnotation("c1=8080;c3=8082"),
				readinessProbe(&corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Port: intstr.FromInt(8082),
						},
					},
				}, 2),
			),
			svc:            testService(8082),
			parallelReqs:   1,
			sequentialReqs: 1,
			maxReqDuration: time.Second,
		},
		"parallel requests": {
			pod:            testPod(defaultScaleDownAfter),
			parallelReqs:   10,
			sequentialReqs: 5,
			maxReqDuration: time.Second,
			waitScaledDown: true,
		},
		"parallel requests with keepalive": {
			pod:            testPod(defaultScaleDownAfter),
			parallelReqs:   10,
			sequentialReqs: 5,
			keepAlive:      true,
			maxReqDuration: time.Second,
			waitScaledDown: true,
		},
		"pod with HTTP probe": {
			pod: testPod(
				scaleDownAfter(time.Second),
				addContainer("nginx", "nginx", nil, 80),
				livenessProbe(&corev1.Probe{
					InitialDelaySeconds: 5,
					PeriodSeconds:       1,
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Port: intstr.FromInt(80),
						},
					},
				}),
			),
			parallelReqs:     0,
			sequentialReqs:   0,
			waitScaledDown:   true,
			expectRunning:    false,
			expectScaledDown: true,
		},
		"pod with TCP probe": {
			pod: testPod(
				scaleDownAfter(time.Second),
				addContainer("nginx", "nginx", nil, 80),
				livenessProbe(&corev1.Probe{
					InitialDelaySeconds: 5,
					PeriodSeconds:       1,
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{
							Port: intstr.FromInt(80),
						},
					},
				}),
			),
			parallelReqs:     0,
			sequentialReqs:   0,
			waitScaledDown:   true,
			expectRunning:    false,
			expectScaledDown: true,
		},
		"pod with large HTTP probe and increased buffer": {
			pod: testPod(
				scaleDownAfter(time.Second),
				annotations(map[string]string{shim.ProbeBufferSizeAnnotationKey: "2048"}),
				addContainer("nginx", "nginx", nil, 80),
				livenessProbe(&corev1.Probe{
					InitialDelaySeconds: 3,
					PeriodSeconds:       1,
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Port: intstr.FromInt(80),
							// ensures probe request is bigger than 1024 bytes
							Path: "/" + strings.Repeat("a", 1025),
						},
					},
				}),
			),
			parallelReqs:     0,
			sequentialReqs:   0,
			waitScaledDown:   true,
			expectRunning:    false,
			expectScaledDown: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.preDump && runtime.GOARCH == "arm64" {
				t.Skip("skipping pre-dump test as it's not supported on arm64")
			}

			if tc.svc == nil {
				tc.svc = testService(defaultTargetPort)
			}

			cleanupPod := createPodAndWait(t, ctx, e2e.client, tc.pod)
			cleanupService := createServiceAndWait(t, ctx, e2e.client, tc.svc, 1)
			defer cleanupPod()
			defer cleanupService()

			if tc.waitScaledDown {
				waitUntilScaledDown(t, ctx, e2e.client, tc.pod)
			}

			if tc.expectRunning {
				alwaysRunningFor(t, ctx, e2e.client, tc.pod, time.Second*10)
			}

			if tc.expectScaledDown {
				alwaysScaledDownFor(t, ctx, e2e.client, tc.pod, time.Second*10)
			}

			wg := sync.WaitGroup{}
			wg.Add(tc.parallelReqs)
			for i := 0; i < tc.parallelReqs; i++ {
				go func() {
					defer wg.Done()
					for x := 0; x < tc.sequentialReqs; x++ {
						defer time.Sleep(tc.sequentialWait)
						c.Transport = &http.Transport{DisableKeepAlives: !tc.keepAlive}

						before := time.Now()
						resp, err := c.Get(fmt.Sprintf("http://localhost:%d", e2e.port))
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
		pod := testPod(scaleDownAfter(time.Second))
		cleanupPod := createPodAndWait(t, ctx, e2e.client, pod)
		defer cleanupPod()
		waitUntilScaledDown(t, ctx, e2e.client, pod)

		stdout, stderr, err := podExec(e2e.cfg, pod, "date")
		require.NoError(t, err)
		t.Log(stdout, stderr)

		require.Eventually(t, func() bool {
			count, err := restoreCount(t, ctx, e2e.client, e2e.cfg, pod)
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
		pod := testPod(scaleDownAfter(time.Second))
		cleanupPod := createPodAndWait(t, ctx, e2e.client, pod)
		defer cleanupPod()
		waitUntilScaledDown(t, ctx, e2e.client, pod)

		stdout, stderr, err := podExec(e2e.cfg, pod, "date")
		require.NoError(t, err)
		t.Log(stdout, stderr)
		// since the cleanup has been deferred it's called right after the
		// exec and should test the deletion in the restored state.
	})

	t.Run("resources scaling", func(t *testing.T) {
		pod := testPod(scaleDownAfter(time.Second), agnContainer("agn", 8080), resources(corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			},
		}))

		cleanupPod := createPodAndWait(t, ctx, e2e.client, pod)
		defer cleanupPod()
		require.Eventually(t, func() bool {
			if err := e2e.client.Get(ctx, objectName(pod), pod); err != nil {
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

	t.Run("status labels", func(t *testing.T) {
		pod := testPod(scaleDownAfter(time.Second), agnContainer("agn", 8080), agnContainer("agn2", 8081))

		cleanupPod := createPodAndWait(t, ctx, e2e.client, pod)
		defer cleanupPod()
		require.Eventually(t, func() bool {
			if err := e2e.client.Get(ctx, objectName(pod), pod); err != nil {
				return false
			}
			labelCount := 0
			expectedLabels := 2
			for k, v := range pod.GetLabels() {
				if strings.HasPrefix(k, manager.StatusLabelKeyPrefix) {
					if v == v1.ContainerPhase_SCALED_DOWN.String() {
						labelCount++
					}
				}
			}

			return labelCount == expectedLabels
		}, time.Minute, time.Second)
	})

	t.Run("socket tracker ignores probe", func(t *testing.T) {
		pod := testPod(
			defaultScaleDownAfter,
			// we use agn as it uses a v6 TCP socket so we can test ipv4 mapped v6 addresses
			agnContainer("agn", 8080),
			livenessProbe(&corev1.Probe{
				PeriodSeconds: 1,
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Port: intstr.FromInt(8080),
					},
				},
			}),
		)
		cleanupPod := createPodAndWait(t, ctx, e2e.client, pod)
		cleanupService := createServiceAndWait(t, ctx, e2e.client, testService(8080), 1)
		defer cleanupPod()
		defer cleanupService()
		// we expect it to scale down even though a constant livenessProbe is
		// hitting it
		waitUntilScaledDown(t, ctx, e2e.client, pod)
		// make a real request and expect it to scale down again
		resp, err := c.Get(fmt.Sprintf("http://localhost:%d", e2e.port))
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		waitUntilScaledDown(t, ctx, e2e.client, pod)
	})

	t.Run("status events", func(t *testing.T) {
		pod := testPod(scaleDownAfter(time.Second), agnContainer("agn", 8080))

		cleanupPod := createPodAndWait(t, ctx, e2e.client, pod)
		defer cleanupPod()
		waitUntilScaledDown(t, ctx, e2e.client, pod)
		_, _, err := podExec(e2e.cfg, pod, "date")
		require.NoError(t, err)

		require.Eventually(t, func() bool {
			eventList := &corev1.EventList{}
			if err := e2e.client.List(ctx, eventList); err != nil {
				return false
			}
			eventCount := 0
			expectedEvents := 2
			for _, event := range eventList.Items {
				if event.ReportingController == manager.ControllerName &&
					event.InvolvedObject.UID == pod.UID {
					eventCount++
				}
			}

			return eventCount == expectedEvents
		}, time.Minute, time.Second, "2 matching events are expected")
	})

	t.Run("metrics", func(t *testing.T) {
		// create two pods to test metric merging
		runningPod := testPod(scaleDownAfter(time.Hour))
		cleanupRunningPod := createPodAndWait(t, ctx, e2e.client, runningPod)
		defer cleanupRunningPod()

		checkpointedPod := testPod(scaleDownAfter(time.Second))
		cleanupCheckpointedPod := createPodAndWait(t, ctx, e2e.client, checkpointedPod)
		defer cleanupCheckpointedPod()

		restoredPod := testPod(scaleDownAfter(time.Second))
		cleanupRestoredPod := createPodAndWait(t, ctx, e2e.client, restoredPod)
		defer cleanupRestoredPod()
		waitUntilScaledDown(t, ctx, e2e.client, restoredPod)

		// exec into pod to ensure it has been restored at least once
		require.Eventually(t, func() bool {
			_, _, err := podExec(e2e.cfg, restoredPod, "date")
			if err != nil {
				t.Logf("error during pod exec: %s", err)
				return false
			}
			return true
		}, time.Minute, time.Second)
		waitUntilScaledDown(t, ctx, e2e.client, restoredPod)

		mfs := map[string]*dto.MetricFamily{}
		require.Eventually(t, func() bool {
			var err error
			mfs, err = getNodeMetrics(ctx, e2e.client, e2e.cfg)
			return err == nil
		}, time.Minute, time.Second)

		tests := map[string]struct {
			metric                  string
			pod                     *corev1.Pod
			gaugeValue              *float64
			minHistogramSampleCount *uint64
		}{
			"running": {
				metric:     prometheus.BuildFQName(manager.MetricsNamespace, "", manager.MetricRunning),
				gaugeValue: ptr.To(float64(1)),
				pod:        runningPod,
			},
			"not running": {
				metric:     prometheus.BuildFQName(manager.MetricsNamespace, "", manager.MetricRunning),
				gaugeValue: ptr.To(float64(0)),
				pod:        checkpointedPod,
			},
			"last checkpoint time": {
				metric: prometheus.BuildFQName(manager.MetricsNamespace, "", manager.MetricLastCheckpointTime),
				pod:    checkpointedPod,
			},
			"checkpoint duration": {
				metric:                  prometheus.BuildFQName(manager.MetricsNamespace, "", manager.MetricCheckpointDuration),
				pod:                     checkpointedPod,
				minHistogramSampleCount: ptr.To(uint64(1)),
			},
			"last restore time": {
				metric: prometheus.BuildFQName(manager.MetricsNamespace, "", manager.MetricLastRestoreTime),
				pod:    restoredPod,
			},
			"restore duration": {
				metric:                  prometheus.BuildFQName(manager.MetricsNamespace, "", manager.MetricRestoreDuration),
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
					manager.LabelPodName:      tc.pod.Name,
					manager.LabelPodNamespace: tc.pod.Namespace,
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
