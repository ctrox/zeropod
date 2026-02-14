package manager

import (
	"context"
	"log/slog"
	"path"

	v1 "github.com/ctrox/zeropod/api/shim/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	StatusLabelKeyPrefix = "status.zeropod.ctrox.dev"
)

type PodLabeller struct {
	log *slog.Logger
}

func NewPodLabeller(log *slog.Logger) *PodLabeller {
	log = log.With("component", "podlabeller")
	log.Info("init")
	return &PodLabeller{log: log}
}

func (pl *PodLabeller) Handle(ctx context.Context, status *v1.ContainerStatus, pod *corev1.Pod) error {
	clog := pl.log.With("container", status.Name, "pod", status.PodName,
		"namespace", status.PodNamespace, "phase", status.Phase.String(),
		"duration", status.EventDuration.AsDuration().String())
	switch status.Phase {
	case v1.ContainerPhase_RUNNING, v1.ContainerPhase_SCALED_DOWN:
		clog.Info("status event")
		pl.setLabel(pod, status)
	}
	return nil
}

func (pu *PodLabeller) setLabel(pod *corev1.Pod, status *v1.ContainerStatus) {
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[path.Join(StatusLabelKeyPrefix, status.Name)] = status.Phase.String()
}
