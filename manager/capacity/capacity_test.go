package capacity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestNodeTracker(t *testing.T) {
	tracker := NewNodeTracker()
	assert.Empty(t, tracker.Capacity(corev1.ResourceCPU))
	assert.Empty(t, tracker.Capacity(corev1.ResourceMemory))
	assert.Empty(t, tracker.Requested(corev1.ResourceCPU))
	assert.Empty(t, tracker.Requested(corev1.ResourceMemory))

	cpu, memory := resource.MustParse("8"), resource.MustParse("16Gi")
	tracker.SetCapacity(corev1.ResourceCPU, cpu)
	tracker.SetCapacity(corev1.ResourceMemory, memory)
	assert.Equal(t, cpu, tracker.Capacity(corev1.ResourceCPU))
	assert.Equal(t, memory, tracker.Capacity(corev1.ResourceMemory))

	tracker.SetRequested(corev1.ResourceCPU, cpu)
	tracker.SetRequested(corev1.ResourceMemory, memory)
	assert.Equal(t, cpu, tracker.Requested(corev1.ResourceCPU))
	assert.Equal(t, memory, tracker.Requested(corev1.ResourceMemory))
}

func TestTaint(t *testing.T) {
	for name, tc := range map[string]struct {
		node     *corev1.Node
		hasTaint bool
	}{
		"no taint": {
			node:     &corev1.Node{},
			hasTaint: false,
		},
		"existing taint": {
			node: &corev1.Node{
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:       TaintKey,
							Effect:    corev1.TaintEffectNoSchedule,
							TimeAdded: ptr.To(metav1.Now()),
						},
					},
				},
			},
			hasTaint: true,
		},
		"different taint": {
			node: &corev1.Node{
				Spec: corev1.NodeSpec{
					Taints: []corev1.Taint{
						{
							Key:       "foo.bar/nope",
							Effect:    corev1.TaintEffectNoSchedule,
							TimeAdded: ptr.To(metav1.Now()),
						},
					},
				},
			},
			hasTaint: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ok, timeAdded := HasTaint(tc.node)
			assert.Equal(t, tc.hasTaint, ok)
			if tc.hasTaint {
				assert.NotEmpty(t, timeAdded)
				assert.False(t, AddTaint(tc.node))
			} else {
				assert.Empty(t, timeAdded)
				assert.True(t, AddTaint(tc.node))
			}
			RemoveTaint(tc.node)
			ok, _ = HasTaint(tc.node)
			assert.False(t, ok)
		})
	}
}
