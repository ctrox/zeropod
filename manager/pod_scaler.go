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
	// we need to request more than 0 as else the resize is rejected because the
	// QOS Class must not change because of a resize.
	ScaledDownCPU    = resource.MustParse("1m")
	ScaledDownMemory = resource.MustParse("1Ki")
	emptyQuantity    = resource.Quantity{}
)

type containerResource map[string]resource.Quantity

type PodScaler struct {
	log *slog.Logger
}

func NewPodScaler(log *slog.Logger) *PodScaler {
	log = log.With("component", "podscaler")
	log.Info("init")
	return &PodScaler{log: log}
}

func (ps *PodScaler) Handle(ctx context.Context, status *v1.ContainerStatus, pod *corev1.Pod) error {
	return ps.reconcileContainerResources(ctx, pod, status.Name, status.Phase)
}

func (ps *PodScaler) reconcileContainerResources(ctx context.Context, pod *corev1.Pod, name string, phase v1.ContainerPhase) error {
	clog := ps.log.With("container", name, "pod", pod.Name,
		"namespace", pod.Namespace, "phase", phase)

	if err := ps.setAnnotations(pod); err != nil {
		return err
	}

	for i, container := range pod.Spec.Containers {
		if container.Name != name {
			continue
		}

		_, hasCPU := container.Resources.Requests[corev1.ResourceCPU]
		_, hasMemory := container.Resources.Requests[corev1.ResourceMemory]
		if !hasCPU && !hasMemory {
			clog.Debug("ignoring container without resources")
			continue
		}

		initial, err := ps.initialRequests(container, pod.Annotations)
		if err != nil {
			return fmt.Errorf("getting initial requests from pod failed: %w", err)
		}

		current := container.Resources.Requests
		if ps.isUpToDate(initial, current, phase) {
			clog.Debug("container is up to date", "initial", printResources(initial))
			continue
		}

		new := ps.newRequests(initial, current.DeepCopy(), phase)
		pod.Spec.Containers[i].Resources.Requests = new
		clog.Debug("container needs to be updated", "current", printResources(current), "new", printResources(new))
	}

	return nil
}

func (ps *PodScaler) isUpToDate(initial, current corev1.ResourceList, phase v1.ContainerPhase) bool {
	switch phase {
	case v1.ContainerPhase_SCALED_DOWN:
		return (current[corev1.ResourceCPU].Equal(ScaledDownCPU) || current[corev1.ResourceCPU].Equal(emptyQuantity)) &&
			(current[corev1.ResourceMemory].Equal(ScaledDownMemory) || current[corev1.ResourceMemory].Equal(emptyQuantity))
	case v1.ContainerPhase_RUNNING:
		return current[corev1.ResourceCPU].Equal(initial[corev1.ResourceCPU]) &&
			current[corev1.ResourceMemory].Equal(initial[corev1.ResourceMemory])
	default:
		return true
	}
}

func (ps *PodScaler) newRequests(initial, current corev1.ResourceList, phase v1.ContainerPhase) corev1.ResourceList {
	switch phase {
	case v1.ContainerPhase_SCALED_DOWN:
		if !current[corev1.ResourceCPU].Equal(emptyQuantity) {
			current[corev1.ResourceCPU] = ScaledDownCPU
		}
		if !current[corev1.ResourceMemory].Equal(emptyQuantity) {
			current[corev1.ResourceMemory] = ScaledDownMemory
		}
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
	if memoryReq, ok := podAnnotations[MemoryAnnotationKey]; ok {
		if err := json.Unmarshal([]byte(memoryReq), &containerMemory); err != nil {
			return nil, err
		}
	}

	if cpu, ok := containerCPUs[container.Name]; ok && !cpu.Equal(emptyQuantity) {
		initial[corev1.ResourceCPU] = cpu
	}

	if memory, ok := containerMemory[container.Name]; ok && !memory.Equal(emptyQuantity) {
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
