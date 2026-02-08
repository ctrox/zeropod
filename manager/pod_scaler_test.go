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
	defaultRunningCPU, defaultRunningMemory := resource.MustParse("100m"), resource.MustParse("100Mi")

	cases := map[string]struct {
		statusEventPhase v1.ContainerPhase
		beforeEvent      corev1.ResourceList
		expected         corev1.ResourceList
		emptyInitialCPU  bool
		emptyInitialMem  bool
	}{
		"running pod is not updated": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU:    defaultRunningCPU,
				corev1.ResourceMemory: defaultRunningMemory,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    defaultRunningCPU,
				corev1.ResourceMemory: defaultRunningMemory,
			},
		},
		"running pod is not updated cpu only": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			emptyInitialMem:  true,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU: defaultRunningCPU,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU: defaultRunningCPU,
			},
		},
		"running pod is not updated mem only": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			emptyInitialCPU:  true,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceMemory: defaultRunningMemory,
			},
			expected: corev1.ResourceList{
				corev1.ResourceMemory: defaultRunningMemory,
			},
		},
		"running is updated when scaling down": {
			statusEventPhase: v1.ContainerPhase_SCALED_DOWN,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU:    defaultRunningCPU,
				corev1.ResourceMemory: defaultRunningMemory,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU:    ScaledDownCPU,
				corev1.ResourceMemory: ScaledDownMemory,
			},
		},
		"running is updated when scaling down cpu only": {
			statusEventPhase: v1.ContainerPhase_SCALED_DOWN,
			emptyInitialMem:  true,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU: defaultRunningCPU,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU: ScaledDownCPU,
			},
		},
		"running is updated when scaling down mem only": {
			statusEventPhase: v1.ContainerPhase_SCALED_DOWN,
			emptyInitialCPU:  true,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceMemory: defaultRunningMemory,
			},
			expected: corev1.ResourceList{
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
		"scaled down pod is not updated cpu only": {
			statusEventPhase: v1.ContainerPhase_SCALED_DOWN,
			emptyInitialMem:  true,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU: ScaledDownCPU,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU: ScaledDownCPU,
			},
		},
		"scaled down pod is not updated mem only": {
			statusEventPhase: v1.ContainerPhase_SCALED_DOWN,
			emptyInitialCPU:  true,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceMemory: ScaledDownMemory,
			},
			expected: corev1.ResourceList{
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
				corev1.ResourceCPU:    defaultRunningCPU,
				corev1.ResourceMemory: defaultRunningMemory,
			},
		},
		"scaled down pod requests are restored when starting cpu only": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			emptyInitialMem:  true,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceCPU: ScaledDownCPU,
			},
			expected: corev1.ResourceList{
				corev1.ResourceCPU: defaultRunningCPU,
			},
		},
		"scaled down pod requests are restored when starting mem only": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			emptyInitialCPU:  true,
			beforeEvent: corev1.ResourceList{
				corev1.ResourceMemory: ScaledDownMemory,
			},
			expected: corev1.ResourceList{
				corev1.ResourceMemory: defaultRunningMemory,
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ps := &PodScaler{log: slog.Default()}
			resourceList := corev1.ResourceList{}
			if !tc.emptyInitialCPU {
				resourceList[corev1.ResourceCPU] = defaultRunningCPU
			}
			if !tc.emptyInitialMem {
				resourceList[corev1.ResourceMemory] = defaultRunningMemory
			}

			initialPod := newPod(resourceList)
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

			assert.Equal(t, tc.expected, pod.Spec.Containers[0].Resources.Requests)
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
			Containers: []corev1.Container{
				{
					Name: "first-container",
					Resources: corev1.ResourceRequirements{
						Requests: req,
					},
				},
				{
					Name: "second-container",
					Resources: corev1.ResourceRequirements{
						Requests: req,
					},
				},
			},
		},
	}
}
