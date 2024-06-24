package manager

import (
	"context"
	"log/slog"
	"testing"

	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type annotationHandler struct {
	annotations map[string]string
}

func (fh *annotationHandler) Handle(ctx context.Context, status *v1.ContainerStatus, pod *corev1.Pod) error {
	pod.SetAnnotations(fh.annotations)
	return nil
}

func TestOnStatus(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		statusEventPhase v1.ContainerPhase
		beforeEvent      map[string]string
		expected         map[string]string
		podHandlers      []PodHandler
	}{
		"pod is updated when we have a pod handler": {
			statusEventPhase: v1.ContainerPhase_RUNNING,
			podHandlers:      []PodHandler{&annotationHandler{annotations: map[string]string{"new": "annotation"}}},
			beforeEvent:      map[string]string{"some": "annotation"},
			expected:         map[string]string{"new": "annotation"},
		},
	}

	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			client := fake.NewClientBuilder().WithScheme(scheme).Build()
			pod := newPod(nil)
			pod.SetAnnotations(tc.beforeEvent)

			ctx := context.Background()
			if err := client.Create(ctx, pod); err != nil {
				t.Fatal(err)
			}

			sub := subscriber{
				kube:        client,
				log:         slog.Default(),
				podHandlers: tc.podHandlers,
			}
			assert.NoError(t, sub.onStatus(ctx, &v1.ContainerStatus{
				Name:         pod.Spec.Containers[0].Name,
				PodName:      pod.Name,
				PodNamespace: pod.Namespace,
				Phase:        tc.statusEventPhase,
			}))

			if err := client.Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, pod); err != nil {
				t.Fatal(err)
			}

			assert.Equal(t, pod.GetAnnotations(), tc.expected)
		})
	}
}
