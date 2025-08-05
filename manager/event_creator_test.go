package manager

import (
	"context"
	"log/slog"
	"testing"
	"time"

	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEventCreator(t *testing.T) {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		statusPhase        v1.ContainerPhase
		statusDuration     time.Duration
		containerName      string
		expectedReason     string
		expectedMessage    string
		expectEventCreated bool
	}{
		"pod running without duration": {
			containerName:      "m83",
			statusPhase:        v1.ContainerPhase_RUNNING,
			expectEventCreated: false,
		},
		"pod running with duration": {
			containerName:      "asdf",
			statusPhase:        v1.ContainerPhase_RUNNING,
			statusDuration:     time.Second,
			expectEventCreated: true,
			expectedReason:     reasonRunning,
			expectedMessage:    "Restored container asdf in 1s",
		},
		"pod scaled down without duration": {
			containerName:      "c",
			statusPhase:        v1.ContainerPhase_SCALED_DOWN,
			expectEventCreated: true,
			expectedReason:     reasonScaledDown,
			expectedMessage:    "Scaled down container c",
		},
		"pod scaled down with duration": {
			containerName:      "c",
			statusPhase:        v1.ContainerPhase_SCALED_DOWN,
			statusDuration:     time.Second,
			expectEventCreated: true,
			expectedReason:     reasonScaledDown,
			expectedMessage:    "Scaled down container c in 1s",
		},
	} {
		t.Run(name, func(t *testing.T) {
			client := fake.NewClientBuilder().WithScheme(scheme).Build()
			ec := NewEventCreator(slog.Default())
			ec.InjectKubeClient(client)
			ctx := context.Background()
			pod := newPod(nil)
			pod.Spec.Containers[0].Name = tc.containerName
			status := &v1.ContainerStatus{
				Name:         tc.containerName,
				PodName:      pod.Name,
				PodNamespace: pod.Namespace,
				Phase:        tc.statusPhase,
			}
			if tc.statusDuration != 0 {
				status.EventDuration = durationpb.New(tc.statusDuration)
			}
			if err := ec.Handle(ctx, status, pod); err != nil {
				t.Fatal(err)
			}

			eventList := &corev1.EventList{}
			if err := client.List(ctx, eventList); err != nil {
				t.Fatal(err)
			}
			if tc.expectEventCreated {
				require.Len(t, eventList.Items, 1)
				event := eventList.Items[0]
				assert.Equal(t, tc.expectedReason, event.Reason)
				assert.Equal(t, tc.expectedMessage, event.Message)
				assert.Equal(t, pod.Name, event.InvolvedObject.Name)
				assert.Equal(t, pod.Namespace, event.InvolvedObject.Namespace)
				assert.Equal(t, "spec.containers{"+tc.containerName+"}", event.InvolvedObject.FieldPath)
			} else {
				assert.Empty(t, eventList.Items)
			}
		})
	}
}
