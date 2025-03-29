package shim

import (
	v1 "github.com/ctrox/zeropod/api/shim/v1"
)

func newMetrics(cfg *Config, running bool) *v1.ContainerMetrics {
	return &v1.ContainerMetrics{
		Name:         cfg.ContainerName,
		PodName:      cfg.PodName,
		PodNamespace: cfg.PodNamespace,
		Running:      running,
	}
}
