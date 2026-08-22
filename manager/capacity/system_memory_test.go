package capacity

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestSystemMemoryTracker(t *testing.T) {
	tracker := NewSystemMemoryTracker(prometheus.NewRegistry(), "name", 1.0)
	assert.Empty(t, tracker.Capacity(corev1.ResourceCPU))
	assert.NotEmpty(t, tracker.Capacity(corev1.ResourceMemory))
	assert.Empty(t, tracker.Requested(corev1.ResourceCPU))
	assert.NotEmpty(t, tracker.Requested(corev1.ResourceMemory))

	// we expect the system memory to not equal 100Ti. In case that ever becomes
	// real this test will fail but who can be mad about that!
	cpu, memory := resource.MustParse("8"), resource.MustParse("100Ti")
	tracker.SetCapacity(corev1.ResourceCPU, cpu)
	tracker.SetCapacity(corev1.ResourceMemory, memory)
	assert.Equal(t, cpu, tracker.Capacity(corev1.ResourceCPU))
	assert.NotEqual(t, memory, tracker.Capacity(corev1.ResourceMemory))

	tracker.SetRequested(corev1.ResourceCPU, cpu)
	tracker.SetRequested(corev1.ResourceMemory, memory)
	assert.Equal(t, cpu, tracker.Requested(corev1.ResourceCPU))
	assert.NotEqual(t, memory, tracker.Requested(corev1.ResourceMemory))
}
