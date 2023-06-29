package zeropod

import (
	"context"
	"runtime"
	"strconv"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/mitchellh/mapstructure"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const (
	PortAnnotationKey                = "zeropod.ctrox.dev/port"
	ScaleDownDurationAnnotationKey   = "zeropod.ctrox.dev/scaledownduration"
	ContainerNameAnnotationKey       = "zeropod.ctrox.dev/container-name"
	DisableCheckpoiningAnnotationKey = "zeropod.ctrox.dev/disable-checkpointing"
	PreDumpAnnotationKey             = "zeropod.ctrox.dev/pre-dump"

	defaultScaleDownDuration = time.Minute
)

type annotationConfig struct {
	Port                 string `mapstructure:"zeropod.ctrox.dev/port"`
	ScaleDownDuration    string `mapstructure:"zeropod.ctrox.dev/scaledownduration"`
	DisableCheckpointing string `mapstructure:"zeropod.ctrox.dev/disable-checkpointing"`
	ZeropodContainerName string `mapstructure:"zeropod.ctrox.dev/container-name"`
	PreDump              string `mapstructure:"zeropod.ctrox.dev/pre-dump"`
	ContainerName        string `mapstructure:"io.kubernetes.cri.container-name"`
	ContainerType        string `mapstructure:"io.kubernetes.cri.container-type"`
	PodName              string `mapstructure:"io.kubernetes.cri.sandbox-name"`
	PodNamespace         string `mapstructure:"io.kubernetes.cri.sandbox-namespace"`
}

type Config struct {
	Port                 uint16
	ScaleDownDuration    time.Duration
	DisableCheckpointing bool
	PreDump              bool
	ZeropodContainerName string
	ContainerName        string
	ContainerType        string
	PodName              string
	PodNamespace         string
}

// NewConfig uses the annotations from the container spec to create a new
// typed ZeropodConfig config.
func NewConfig(ctx context.Context, spec *specs.Spec) (*Config, error) {
	cfg := &annotationConfig{}
	if err := mapstructure.Decode(spec.Annotations, cfg); err != nil {
		return nil, err
	}

	port, err := strconv.ParseUint(cfg.Port, 10, 16)
	if err != nil {
		return nil, err
	}

	dur := defaultScaleDownDuration
	if len(cfg.ScaleDownDuration) != 0 {
		dur, err = time.ParseDuration(cfg.ScaleDownDuration)
		if err != nil {
			return nil, err
		}
	}

	disableCheckpointing := false
	if len(cfg.DisableCheckpointing) != 0 {
		disableCheckpointing, err = strconv.ParseBool(cfg.DisableCheckpointing)
		if err != nil {
			return nil, err
		}

	}

	preDump := false
	if len(cfg.PreDump) != 0 {
		preDump, err = strconv.ParseBool(cfg.PreDump)
		if err != nil {
			return nil, err
		}
		if preDump && runtime.GOARCH == "arm64" {
			// disable pre-dump on arm64
			// https://github.com/checkpoint-restore/criu/issues/1859
			log.G(ctx).Warnf("disabling pre-dump: it was requested but is not supported on %s", runtime.GOARCH)
			preDump = false
		}
	}

	return &Config{
		Port:                 uint16(port),
		ScaleDownDuration:    dur,
		DisableCheckpointing: disableCheckpointing,
		PreDump:              preDump,
		ZeropodContainerName: cfg.ZeropodContainerName,
		ContainerName:        cfg.ContainerName,
		ContainerType:        cfg.ContainerType,
		PodName:              cfg.PodName,
		PodNamespace:         cfg.PodNamespace,
	}, nil
}
