package capacity

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// NoopTracker implements a capacity tracker that does nothing.
type NoopTracker struct{}

// Capacity implements [Tracker].
func (n *NoopTracker) Capacity(name corev1.ResourceName) resource.Quantity {
	return resource.Quantity{}
}

// Requested implements [Tracker].
func (n *NoopTracker) Requested(name corev1.ResourceName) resource.Quantity {
	return resource.Quantity{}
}

// SetCapacity implements [Tracker].
func (n *NoopTracker) SetCapacity(name corev1.ResourceName, q resource.Quantity) {
}

// SetRequested implements [Tracker].
func (n *NoopTracker) SetRequested(name corev1.ResourceName, q resource.Quantity) {
}

// IncEvicted implements [Tracker].
func (n *NoopTracker) IncEvicted() {
}

// UseCheckpointMemory implements [Tracker].
func (n *NoopTracker) UseCheckpointMemory() bool {
	return false
}

// Threshold implements [Tracker].
func (n *NoopTracker) Threshold() float64 {
	return 1.0
}

// NewNoopTracker creates a [NoopTracker]
func NewNoopTracker() Tracker {
	return &NoopTracker{}
}
