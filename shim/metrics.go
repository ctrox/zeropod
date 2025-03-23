package shim

import (
	"github.com/prometheus/client_golang/prometheus"
)

const (
	labelContainerName = "container"
	LabelPodName       = "pod"
	LabelPodNamespace  = "namespace"

	MetricsNamespace         = "zeropod"
	MetricCheckPointDuration = "checkpoint_duration_seconds"
	MetricRestoreDuration    = "restore_duration_seconds"
	MetricLastCheckpointTime = "last_checkpoint_time"
	MetricLastRestoreTime    = "last_restore_time"
	MetricRunning            = "running"
)

var (
	// buckets used for the checkpoint/restore histograms.
	crBuckets = []float64{
		0.02, 0.03, 0.04, 0.05, 0.075,
		0.1, 0.12, 0.14, 0.16, 0.18,
		0.2, 0.3, 0.4, 0.5, 1,
	}

	commonLabels = []string{labelContainerName, LabelPodName, LabelPodNamespace}

	checkpointDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      MetricCheckPointDuration,
		Help:      "The duration of the last checkpoint in seconds.",
		Buckets:   crBuckets,
	}, commonLabels)

	restoreDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: MetricsNamespace,
		Name:      MetricRestoreDuration,
		Help:      "The duration of the last restore in seconds.",
		Buckets:   crBuckets,
	}, commonLabels)

	lastCheckpointTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      MetricLastCheckpointTime,
		Help:      "A unix timestamp in nanoseconds of the last checkpoint.",
	}, commonLabels)

	lastRestoreTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      MetricLastRestoreTime,
		Help:      "A unix timestamp in nanoseconds of the last restore.",
	}, commonLabels)

	running = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: MetricsNamespace,
		Name:      MetricRunning,
		Help:      "Reports if the process is currently running or checkpointed.",
	}, commonLabels)
)

func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()

	reg.MustRegister(
		checkpointDuration, restoreDuration,
		lastCheckpointTime, lastRestoreTime, running,
	)

	return reg
}

func (c *Container) labels() map[string]string {
	return map[string]string{
		labelContainerName: c.cfg.ContainerName,
		LabelPodName:       c.cfg.PodName,
		LabelPodNamespace:  c.cfg.PodNamespace,
	}
}

func (c *Container) deleteMetrics() {
	checkpointDuration.Delete(c.labels())
	restoreDuration.Delete(c.labels())
	lastCheckpointTime.Delete(c.labels())
	lastRestoreTime.Delete(c.labels())
	running.Delete(c.labels())
}
