package zeropod

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/mitchellh/mapstructure"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const (
	NodeLabel                        = "zeropod.ctrox.dev/node"
	PortsAnnotationKey               = "zeropod.ctrox.dev/ports"
	ScaleDownDurationAnnotationKey   = "zeropod.ctrox.dev/scaledownduration"
	ContainerNamesAnnotationKey      = "zeropod.ctrox.dev/container-names"
	DisableCheckpoiningAnnotationKey = "zeropod.ctrox.dev/disable-checkpointing"
	PreDumpAnnotationKey             = "zeropod.ctrox.dev/pre-dump"

	defaultScaleDownDuration = time.Minute
)

type annotationConfig struct {
	Ports                 string `mapstructure:"zeropod.ctrox.dev/ports"`
	ScaleDownDuration     string `mapstructure:"zeropod.ctrox.dev/scaledownduration"`
	DisableCheckpointing  string `mapstructure:"zeropod.ctrox.dev/disable-checkpointing"`
	ZeropodContainerNames string `mapstructure:"zeropod.ctrox.dev/container-names"`
	PreDump               string `mapstructure:"zeropod.ctrox.dev/pre-dump"`
	ContainerName         string `mapstructure:"io.kubernetes.cri.container-name"`
	ContainerType         string `mapstructure:"io.kubernetes.cri.container-type"`
	PodName               string `mapstructure:"io.kubernetes.cri.sandbox-name"`
	PodNamespace          string `mapstructure:"io.kubernetes.cri.sandbox-namespace"`
}

type Config struct {
	Ports                 []uint16
	ScaleDownDuration     time.Duration
	DisableCheckpointing  bool
	PreDump               bool
	ZeropodContainerNames []string
	ContainerName         string
	ContainerType         string
	PodName               string
	PodNamespace          string
}

// NewConfig uses the annotations from the container spec to create a new
// typed ZeropodConfig config.
func NewConfig(ctx context.Context, spec *specs.Spec) (*Config, error) {
	cfg := &annotationConfig{}
	if err := mapstructure.Decode(spec.Annotations, cfg); err != nil {
		return nil, err
	}

	var err error
	var ports []uint16
	if len(cfg.Ports) != 0 {
		for _, p := range strings.Split(cfg.Ports, ",") {
			port, err := strconv.ParseUint(p, 10, 16)
			if err != nil {
				return nil, err
			}
			ports = append(ports, uint16(port))
		}
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

	containerNames := []string{}
	if len(cfg.ZeropodContainerNames) != 0 {
		containerNames = strings.Split(cfg.ZeropodContainerNames, ",")
	}

	return &Config{
		Ports:                 ports,
		ScaleDownDuration:     dur,
		DisableCheckpointing:  disableCheckpointing,
		PreDump:               preDump,
		ZeropodContainerNames: containerNames,
		ContainerName:         cfg.ContainerName,
		ContainerType:         cfg.ContainerType,
		PodName:               cfg.PodName,
		PodNamespace:          cfg.PodNamespace,
	}, nil
}

func (cfg Config) IsZeropodContainer() bool {
	for _, n := range cfg.ZeropodContainerNames {
		if n == cfg.ContainerName {
			return true
		}
	}

	// if there is none specified, every one of them is considered.
	return len(cfg.ZeropodContainerNames) == 0
}
