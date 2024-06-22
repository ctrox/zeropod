package manager

import (
	"context"
	"log/slog"
	"testing"

	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestHandlePod(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	runningCPU, runningMemory := resource.MustParse("100m"), resource.MustParse("100Mi")

	cases := map[string]struct {
		statusEventPhase v1.ContainerPhase
		beforeEvent      corev1.ResourceList
		expected         corev1.ResourceList
	}{
		"running pod is not updated": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU:    runningCPU,
				corev1.ResourceMemory: runningMemory,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    runningCPU,
				corev1.ResourceMemory: runningMemory,
			},
		},
		"running is updated when scaling down": {
			statusEventPhase: v1.ContainerPhase_SCALED_DOWN,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU:    runningCPU,
				corev1.ResourceMemory: runningMemory,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    ScaledDownCPU,
				corev1.ResourceMemory: ScaledDownMemory,
			},
		},
		"scaled down pod is not updated": {
			statusEventPhase: v1.ContainerPhase_SCALED_DOWN,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU:    ScaledDownCPU,
				corev1.ResourceMemory: ScaledDownMemory,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    ScaledDownCPU,
				corev1.ResourceMemory: ScaledDownMemory,
			},
		},
		"scaled down pod requests are restored when starting": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU:    ScaledDownCPU,
				corev1.ResourceMemory: ScaledDownMemory,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    runningCPU,
				corev1.ResourceMemory: runningMemory,
			},
		},
	}

	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			ps := &PodScaler{log: slog.Default()}

			initialPod := newPod(corev1.ResourceList{corev1.ResourceCPU: runningCPU, corev1.ResourceMemory: runningMemory})
			ps.setAnnotations(initialPod)
			pod := newPod(tc.beforeEvent)
			pod.SetAnnotations(initialPod.GetAnnotations())

			if err := ps.Handle(
				context.Background(),
				&v1.ContainerStatus{
					Name:         pod.Spec.Containers[0].Name,
					PodName:      pod.Name,
					PodNamespace: pod.Namespace,
					Phase:        tc.statusEventPhase,
				},
				pod,
			); err != nil {
				t.Fatal(err)
			}

			assert.Equal(t, pod.Spec.Containers[0].Resources.Requests, tc.expected)
		})
	}
}

func newPod(req corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scaled-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "first-container",
				Resources: corev1.ResourceRequirements{
					Requests: req,
				},
			}},
		},
	}
}
