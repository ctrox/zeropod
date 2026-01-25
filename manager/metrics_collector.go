package manager

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/containerd/ttrpc"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	labelContainerName = "container"
	LabelPodName       = "pod"
	LabelPodNamespace  = "namespace"

	MetricsNamespace         = "zeropod"
	MetricCheckpointDuration = "checkpoint_duration_seconds"
	MetricRestoreDuration    = "restore_duration_seconds"
	MetricLastCheckpointTime = "last_checkpoint_time"
	MetricLastRestoreTime    = "last_restore_time"
	MetricRunning            = "running"
)

var (
	crBuckets = []float64{
		0.025, 0.05, 0.1, 0.2, 0.3,
		0.4, 0.5, 0.6, 0.7, 0.8,
		0.9, 1, 2.5, 5, 10,
	}
	commonLabels = []string{labelContainerName, LabelPodName, LabelPodNamespace}
)

type Collector struct {
	checkpointDuration *prometheus.HistogramVec
	restoreDuration    *prometheus.HistogramVec
	lastCheckpointTime *prometheus.GaugeVec
	lastRestoreTime    *prometheus.GaugeVec
	running            *prometheus.GaugeVec
}

func NewCollector() *Collector {
	return &Collector{
		checkpointDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: MetricsNamespace,
			Name:      MetricCheckpointDuration,
			Help:      "The duration of the last checkpoint in seconds.",
			Buckets:   crBuckets,
		}, commonLabels),

		restoreDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: MetricsNamespace,
			Name:      MetricRestoreDuration,
			Help:      "The duration of the last restore in seconds.",
			Buckets:   crBuckets,
		}, commonLabels),

		lastCheckpointTime: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      MetricLastCheckpointTime,
			Help:      "A unix timestamp in nanoseconds of the last checkpoint.",
		}, commonLabels),

		lastRestoreTime: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      MetricLastRestoreTime,
			Help:      "A unix timestamp in nanoseconds of the last restore.",
		}, commonLabels),

		running: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: MetricsNamespace,
			Name:      MetricRunning,
			Help:      "Reports if the process is currently running or checkpointed.",
		}, commonLabels),
	}
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	socks, err := os.ReadDir(v1.ShimSocketPath)
	if err != nil {
		slog.Error("error listing file in shim socket path", "path", v1.ShimSocketPath, "err", err)
		return
	}

	for _, sock := range socks {
		sockName := filepath.Join(v1.ShimSocketPath, sock.Name())
		slog.Debug("getting metrics", "name", sockName)

		shimMetrics, err := collectMetricsOverTTRPC(context.Background(), sockName)
		if err != nil {
			slog.Error("getting metrics", "err", err)
			// we still want to read the rest of the sockets
			continue
		}

		for _, metrics := range shimMetrics {
			l := labels(metrics)
			r := 0
			if metrics.Running {
				r = 1
			}
			// TODO: handle stale metrics
			c.running.With(l).Set(float64(r))
			if metrics.LastCheckpointDuration != nil {
				c.checkpointDuration.With(l).Observe(metrics.LastCheckpointDuration.AsDuration().Seconds())
			}
			if metrics.LastRestoreDuration != nil {
				c.restoreDuration.With(l).Observe(metrics.LastRestoreDuration.AsDuration().Seconds())
			}
			if metrics.LastCheckpoint != nil {
				c.lastCheckpointTime.With(l).Set(float64(metrics.LastCheckpoint.AsTime().UnixNano()))
			}
			if metrics.LastRestore != nil {
				c.lastRestoreTime.With(l).Set(float64(metrics.LastRestore.AsTime().UnixNano()))
			}
		}
	}
	c.running.Collect(ch)
	c.checkpointDuration.Collect(ch)
	c.restoreDuration.Collect(ch)
	c.lastCheckpointTime.Collect(ch)
	c.lastRestoreTime.Collect(ch)
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
}

func collectMetricsOverTTRPC(ctx context.Context, sock string) ([]*v1.ContainerMetrics, error) {
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}

	resp, err := v1.NewShimClient(ttrpc.NewClient(conn)).Metrics(ctx, &v1.MetricsRequest{})
	if err != nil {
		return nil, err
	}

	return resp.Metrics, nil
}

func labels(metrics *v1.ContainerMetrics) map[string]string {
	return map[string]string{
		labelContainerName: metrics.Name,
		LabelPodName:       metrics.PodName,
		LabelPodNamespace:  metrics.PodNamespace,
	}
}

func (c *Collector) deleteMetrics(status *v1.ContainerStatus) {
	l := labels(&v1.ContainerMetrics{
		Name:         status.Name,
		PodName:      status.PodName,
		PodNamespace: status.PodNamespace,
	})
	c.running.Delete(l)
	c.checkpointDuration.Delete(l)
	c.restoreDuration.Delete(l)
	c.lastCheckpointTime.Delete(l)
	c.lastRestoreTime.Delete(l)
}
