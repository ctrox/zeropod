package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	v1 "github.com/ctrox/zeropod/api/shim/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	CPUAnnotationKey    = "zeropod.ctrox.dev/cpu-requests"
	MemoryAnnotationKey = "zeropod.ctrox.dev/memory-requests"
)

var (
	ScaledDownCPU    = resource.MustParse("1m")
	ScaledDownMemory = resource.MustParse("1Ki")
)

type containerResource map[string]resource.Quantity

type PodScaler struct {
	log *slog.Logger
}

func NewPodScaler() *PodScaler {
	log := slog.With("component", "podscaler")
	log.Info("init")
	return &PodScaler{log: log}
}

func (ps *PodScaler) Handle(ctx context.Context, status *v1.ContainerStatus, pod *corev1.Pod) error {
	clog := ps.log.With("container", status.Name, "pod", status.PodName,
		"namespace", status.PodNamespace, "phase", status.Phase)
	clog.Info("status event")

	for i, container := range pod.Spec.Containers {
		if container.Name != status.Name {
			continue
		}

		_, hasCPU := container.Resources.Requests[corev1.ResourceCPU]
		_, hasMemory := container.Resources.Requests[corev1.ResourceMemory]
		if !hasCPU || !hasMemory {
			clog.Debug("ignoring container without resources")
			continue
		}

		initial, err := ps.initialRequests(container, pod.Annotations)
		if err != nil {
			return fmt.Errorf("getting initial requests from pod failed: %w", err)
		}

		current := container.Resources.Requests
		if ps.isUpToDate(initial, current, status) {
			clog.Debug("container is up to date", "initial", printResources(initial))
			continue
		}

		if err := ps.setAnnotations(pod); err != nil {
			return err
		}

		new := ps.newRequests(initial, current, status)
		pod.Spec.Containers[i].Resources.Requests = new
		clog.Debug("container needs to be updated", "current", printResources(current), "new", printResources(new))
	}

	return nil
}

func (ps *PodScaler) isUpToDate(initial, current corev1.ResourceList, status *v1.ContainerStatus) bool {
	switch status.Phase {
	case v1.ContainerPhase_SCALED_DOWN:
		return current[corev1.ResourceCPU] == ScaledDownCPU &&
			current[corev1.ResourceMemory] == ScaledDownMemory
	case v1.ContainerPhase_RUNNING:
		return current[corev1.ResourceCPU] == initial[corev1.ResourceCPU] &&
			current[corev1.ResourceMemory] == initial[corev1.ResourceMemory]
	default:
		return true
	}
}

func (ps *PodScaler) newRequests(initial, current corev1.ResourceList, status *v1.ContainerStatus) corev1.ResourceList {
	switch status.Phase {
	case v1.ContainerPhase_SCALED_DOWN:
		current[corev1.ResourceCPU] = ScaledDownCPU
		current[corev1.ResourceMemory] = ScaledDownMemory
		return current
	case v1.ContainerPhase_RUNNING:
		return initial
	default:
		return current
	}
}

func (ps *PodScaler) initialRequests(container corev1.Container, podAnnotations map[string]string) (corev1.ResourceList, error) {
	initial := container.DeepCopy().Resources.Requests
	containerCPUs := containerResource{}
	if cpuReq, ok := podAnnotations[CPUAnnotationKey]; ok {
		if err := json.Unmarshal([]byte(cpuReq), &containerCPUs); err != nil {
			return nil, err
		}
	}

	containerMemory := containerResource{}
	if memortReq, ok := podAnnotations[MemoryAnnotationKey]; ok {
		if err := json.Unmarshal([]byte(memortReq), &containerMemory); err != nil {
			return nil, err
		}
	}

	if cpu, ok := containerCPUs[container.Name]; ok {
		initial[corev1.ResourceCPU] = cpu
	}

	if memory, ok := containerMemory[container.Name]; ok {
		initial[corev1.ResourceMemory] = memory
	}

	return initial, nil
}

func (ps *PodScaler) setAnnotations(pod *corev1.Pod) error {
	containerCPUs := containerResource{}
	containerMemory := containerResource{}
	for _, container := range pod.Spec.Containers {
		containerCPUs[container.Name] = container.Resources.Requests[corev1.ResourceCPU]
		containerMemory[container.Name] = container.Resources.Requests[corev1.ResourceMemory]
	}

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	if _, ok := pod.Annotations[CPUAnnotationKey]; !ok {
		val, err := json.Marshal(containerCPUs)
		if err != nil {
			return err
		}
		pod.Annotations[CPUAnnotationKey] = string(val)
	}

	if _, ok := pod.Annotations[MemoryAnnotationKey]; !ok {
		val, err := json.Marshal(containerMemory)
		if err != nil {
			return err
		}
		pod.Annotations[MemoryAnnotationKey] = string(val)
	}

	return nil
}

func printResources(res corev1.ResourceList) string {
	cpu := res[corev1.ResourceCPU]
	memory := res[corev1.ResourceMemory]
	return fmt.Sprintf("cpu: %s, memory: %s", cpu.String(), memory.String())
}
