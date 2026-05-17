// Package capacity implements a tracker for node resource capacity.
package capacity

import (
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	TaintKey         = "zeropod.ctrox.dev/at-capacity"
	MinTaintDuration = time.Minute

	metricsNamespace    = "zeropod"
	MetricNodeCapacity  = "node_capacity"
	MetricNodeRequested = "node_requested"
	MetricNodeEvictions = "node_evictions"

	labelNode     = "node"
	labelResource = "resource"
)

type Tracker interface {
	Capacity(name corev1.ResourceName) resource.Quantity
	Requested(name corev1.ResourceName) resource.Quantity
	SetCapacity(name corev1.ResourceName, q resource.Quantity)
	SetRequested(name corev1.ResourceName, q resource.Quantity)
	IncEvicted()
}

type NodeTracker struct {
	name      string
	capacity  corev1.ResourceList
	requested corev1.ResourceList
	mu        sync.RWMutex
	metrics   metrics
}

type metrics struct {
	capacity  *prometheus.GaugeVec
	requested *prometheus.GaugeVec
	evicted   *prometheus.CounterVec
}

// Capacity returns the node capacity for the [corev1.ResourceName].
func (c *NodeTracker) Capacity(name corev1.ResourceName) resource.Quantity {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.capacity[name]
}

// Requested returns the node requested resources for the [corev1.ResourceName].
func (c *NodeTracker) Requested(name corev1.ResourceName) resource.Quantity {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.requested[name]
}

// SetCapacity sets the resource capacity of a node.
func (c *NodeTracker) SetCapacity(name corev1.ResourceName, q resource.Quantity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metrics.capacity.With(c.metricLabels(name.String())).Set(q.AsApproximateFloat64())
	c.capacity[name] = q
}

// SetRequested sets the requested resources of a node.
func (c *NodeTracker) SetRequested(name corev1.ResourceName, q resource.Quantity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metrics.requested.With(c.metricLabels(name.String())).Set(q.AsApproximateFloat64())
	c.requested[name] = q
}

func (c *NodeTracker) IncEvicted() {
	c.metrics.evicted.With(prometheus.Labels{labelNode: c.name}).Inc()
}

func (c *NodeTracker) metricLabels(resource string) prometheus.Labels {
	return prometheus.Labels{labelNode: c.name, labelResource: resource}
}

// NewNodeTracker creates a [NodeTracker].
func NewNodeTracker(reg prometheus.Registerer, name string) Tracker {
	return &NodeTracker{
		name:      name,
		capacity:  corev1.ResourceList{},
		requested: corev1.ResourceList{},
		mu:        sync.RWMutex{},
		metrics: metrics{
			capacity: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Name:      MetricNodeCapacity,
				Help:      "Node resource capacity.",
			}, []string{labelNode, labelResource}),
			requested: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
				Namespace: metricsNamespace,
				Name:      MetricNodeRequested,
				Help:      "Node resources requested.",
			}, []string{labelNode, labelResource}),
			evicted: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
				Namespace: metricsNamespace,
				Name:      MetricNodeEvictions,
				Help:      "Counts node capacity evictions.",
			}, []string{labelNode}),
		},
	}
}

// AddTaint taints the node to not be used for scheduling if not already the case.
func AddTaint(node *corev1.Node) bool {
	if slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.Key == TaintKey
	}) {
		return false
	}
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key:       TaintKey,
		Effect:    corev1.TaintEffectNoSchedule,
		TimeAdded: ptr.To(metav1.Now()),
	})
	return true
}

// HasTaint checks for an existing taint and returns the time added if true.
func HasTaint(node *corev1.Node) (bool, time.Time) {
	var taintAdded time.Time
	if slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool {
		ok := t.Key == TaintKey
		if ok && t.TimeAdded != nil {
			taintAdded = t.TimeAdded.Time
		}
		return ok
	}) {
		return true, taintAdded
	}
	return false, taintAdded
}

// RemoveTaint removes the taint from the node.
func RemoveTaint(node *corev1.Node) {
	node.Spec.Taints = slices.DeleteFunc(node.Spec.Taints, func(t corev1.Taint) bool {
		return t.Key == TaintKey
	})
}
