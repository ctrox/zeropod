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
	PortAnnotationKey              = "zeropod.ctrox.dev/port"
	ScaleDownDurationAnnotationKey = "zeropod.ctrox.dev/scaledownduration"
	ContainerNameAnnotationKey     = "zeropod.ctrox.dev/container-name"
	StatefulAnnotationKey          = "zeropod.ctrox.dev/stateful"
	PreDumpAnnotationKey           = "zeropod.ctrox.dev/pre-dump"
)

type annotationConfig struct {
	Port                 string `mapstructure:"zeropod.ctrox.dev/port"`
	ScaleDownDuration    string `mapstructure:"zeropod.ctrox.dev/scaledownduration"`
	Stateful             string `mapstructure:"zeropod.ctrox.dev/stateful"`
	ZeropodContainerName string `mapstructure:"zeropod.ctrox.dev/container-name"`
	PreDump              string `mapstructure:"zeropod.ctrox.dev/pre-dump"`
	ContainerName        string `mapstructure:"io.kubernetes.cri.container-name"`
	ContainerType        string `mapstructure:"io.kubernetes.cri.container-type"`
}

type Config struct {
	Port                 uint16
	ScaleDownDuration    time.Duration
	Stateful             bool
	PreDump              bool
	ZeropodContainerName string
	ContainerName        string
	ContainerType        string
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

	dur, err := time.ParseDuration(cfg.ScaleDownDuration)
	if err != nil {
		return nil, err
	}

	// we default to stateful if it's not set
	stateful := true
	if len(cfg.Stateful) != 0 {
		stateful, err = strconv.ParseBool(cfg.Stateful)
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
		Stateful:             stateful,
		PreDump:              preDump,
		ZeropodContainerName: cfg.ZeropodContainerName,
		ContainerName:        cfg.ContainerName,
		ContainerType:        cfg.ContainerType,
	}, nil
}
