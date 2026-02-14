package manager

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ControllerName         = "zeropod.ctrox.dev/manager"
	eventNamePrefix        = "zeropod-status-event-"
	reasonRunning          = "Running"
	reasonScaledDown       = "Scaled down"
	reasonCheckpointFailed = "Checkpoint failed"
	reasonRestoreFailed    = "Restore failed"
)

type EventCreator struct {
	log    *slog.Logger
	client client.Client
}

func NewEventCreator(log *slog.Logger) *EventCreator {
	log = log.With("component", "event_creator")
	log.Info("init")
	return &EventCreator{log: log}
}

func (ec *EventCreator) InjectKubeClient(c client.Client) {
	ec.client = c
}

func (ec *EventCreator) Handle(ctx context.Context, status *v1.ContainerStatus, pod *corev1.Pod) error {
	clog := ec.log.With("container", status.Name, "pod", status.PodName,
		"namespace", status.PodNamespace, "phase", status.Phase.String(),
		"duration", status.EventDuration.AsDuration().String())
	clog.Info("status event")

	message, reason := "", ""
	switch status.Phase {
	case v1.ContainerPhase_RUNNING:
		reason = reasonRunning
		// don't create an event if container was simply started without being restored
		if status.EventDuration == nil || status.EventDuration.AsDuration() == 0 {
			return nil
		}
		message = fmt.Sprintf("Restored container %s in %s", status.Name, status.EventDuration.AsDuration())
	case v1.ContainerPhase_SCALED_DOWN:
		reason = reasonScaledDown
		message = fmt.Sprintf("Scaled down container %s", status.Name)
		if status.EventDuration != nil {
			message += " in " + status.EventDuration.AsDuration().String()
		}
	case v1.ContainerPhase_CHECKPOINT_FAILED:
		reason = reasonCheckpointFailed
		message = fmt.Sprintf("Checkpoint failed for container %s", status.Name)
	case v1.ContainerPhase_RESTORE_FAILED:
		reason = reasonRestoreFailed
		message = fmt.Sprintf("Restore failed for container %s", status.Name)
	}

	instance, err := os.Hostname()
	if err != nil {
		// fall back to nodename as instance can't be empty
		instance = os.Getenv(nodev1.NodeNameEnvKey)
	}
	return ec.client.Create(ctx, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: eventNamePrefix,
			Namespace:    pod.Namespace,
		},
		Reason:         reason,
		EventTime:      metav1.NewMicroTime(status.EventTime.AsTime()),
		FirstTimestamp: metav1.NewTime(status.EventTime.AsTime()),
		LastTimestamp:  metav1.NewTime(status.EventTime.AsTime()),
		Message:        message,
		Type:           corev1.EventTypeNormal,
		InvolvedObject: corev1.ObjectReference{
			APIVersion:      corev1.SchemeGroupVersion.Version,
			Kind:            "Pod",
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			ResourceVersion: pod.ResourceVersion,
			UID:             pod.UID,
			FieldPath:       fmt.Sprintf("spec.containers{%s}", status.Name),
		},
		ReportingController: ControllerName,
		ReportingInstance:   instance,
		Action:              reason,
		Source: corev1.EventSource{
			Component: ControllerName,
			Host:      os.Getenv(nodev1.NodeNameEnvKey),
		},
	})
}
