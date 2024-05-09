package zeropod

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

const MetricsSocketPath = "/run/zeropod/s/"

func metricsSocketAddress(containerID string) string {
	return fmt.Sprintf("unix://%s.sock", filepath.Join(MetricsSocketPath, containerID))
}

func StartMetricsServer(ctx context.Context, containerID string) {
	metricsAddress := metricsSocketAddress(containerID)
	listener, err := shim.NewSocket(metricsAddress)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			log.G(ctx).WithError(err)
			return
		}

		if shim.CanConnect(metricsAddress) {
			log.G(ctx).Debug("metrics socket already exists, skipping server start")
			return
		}

		if err := shim.RemoveSocket(metricsAddress); err != nil {
			log.G(ctx).WithError(fmt.Errorf("remove pre-existing socket: %w", err))
		}

		listener, err = shim.NewSocket(metricsAddress)
		if err != nil {
			log.G(ctx).WithError(err).Error("failed to create metrics listener")
		}
	}

	log.G(ctx).Infof("starting metrics server at %s", metricsAddress)
	// write metrics address to filesystem
	if err := shim.WriteAddress("metrics_address", metricsAddress); err != nil {
		log.G(ctx).WithError(err).Errorf("failed to write metrics address")
		return
	}

	mux := http.NewServeMux()
	handler := promhttp.HandlerFor(
		newRegistry(),
		promhttp.HandlerOpts{
			EnableOpenMetrics: false,
		},
	)

	mux.Handle("/metrics", handler)

	server := http.Server{Handler: mux}

	go server.Serve(listener)

	<-ctx.Done()

	log.G(ctx).Info("stopping metrics server")
	listener.Close()
	server.Close()
	_ = os.RemoveAll(metricsAddress)
}

func newRegistry() *prometheus.Registry {
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
