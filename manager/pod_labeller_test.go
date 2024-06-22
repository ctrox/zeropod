package manager

import (
	"context"
	"log/slog"
	"testing"

	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestPodLabeller(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		statusEventPhase v1.ContainerPhase
		beforeEvent      map[string]string
		expected         map[string]string
	}{
		"no labels set": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			beforeEvent:      nil,
			expected: map[string]string{
				"status.zeropod.ctrox.dev/first-container": v1.ContainerPhase_RUNNING.String(),
			},
		},
		"existing labels are kept": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			beforeEvent:      map[string]string{"existing": "label"},
			expected: map[string]string{
				"existing": "label",
				"status.zeropod.ctrox.dev/first-container": v1.ContainerPhase_RUNNING.String(),
			},
		},
		"status label is updated": {
			statusEventPhase: v1.ContainerPhase_SCALED_DOWN,
			beforeEvent: map[string]string{
				"status.zeropod.ctrox.dev/first-container": v1.ContainerPhase_RUNNING.String(),
			},
			expected: map[string]string{
				"status.zeropod.ctrox.dev/first-container": v1.ContainerPhase_SCALED_DOWN.String(),
			},
		},
	}

	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			pod := newPod(nil)
			pod.SetLabels(tc.beforeEvent)

			if err := NewPodLabeller().Handle(
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

			assert.Equal(t, pod.GetLabels(), tc.expected)
		})
	}
}
