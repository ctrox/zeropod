package shim

import (
	"context"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log"
	"github.com/mitchellh/mapstructure"
	"github.com/opencontainers/runtime-spec/specs-go"
)

const (
	NodeLabel                        = "zeropod.ctrox.dev/node"
	PortsAnnotationKey               = "zeropod.ctrox.dev/ports-map"
	ContainerNamesAnnotationKey      = "zeropod.ctrox.dev/container-names"
	ScaleDownDurationAnnotationKey   = "zeropod.ctrox.dev/scaledown-duration"
	DisableCheckpoiningAnnotationKey = "zeropod.ctrox.dev/disable-checkpointing"
	PreDumpAnnotationKey             = "zeropod.ctrox.dev/pre-dump"
	MigrateAnnotationKey             = "zeropod.ctrox.dev/migrate"
	LiveMigrateAnnotationKey         = "zeropod.ctrox.dev/live-migrate"
	CRIContainerNameAnnotation       = "io.kubernetes.cri.container-name"
	CRIContainerTypeAnnotation       = "io.kubernetes.cri.container-type"

	defaultScaleDownDuration = time.Minute
	containersDelim          = ","
	portsDelim               = containersDelim
	mappingDelim             = ";"
	mapDelim                 = "="
	defaultContainerdNS      = "k8s.io"
)

type annotationConfig struct {
	PortMap               string `mapstructure:"zeropod.ctrox.dev/ports-map"`
	ZeropodContainerNames string `mapstructure:"zeropod.ctrox.dev/container-names"`
	ScaledownDuration     string `mapstructure:"zeropod.ctrox.dev/scaledown-duration"`
	DisableCheckpointing  string `mapstructure:"zeropod.ctrox.dev/disable-checkpointing"`
	PreDump               string `mapstructure:"zeropod.ctrox.dev/pre-dump"`
	Migrate               string `mapstructure:"zeropod.ctrox.dev/migrate"`
	LiveMigrate           string `mapstructure:"zeropod.ctrox.dev/live-migrate"`
	ContainerName         string `mapstructure:"io.kubernetes.cri.container-name"`
	ContainerType         string `mapstructure:"io.kubernetes.cri.container-type"`
	PodName               string `mapstructure:"io.kubernetes.cri.sandbox-name"`
	PodNamespace          string `mapstructure:"io.kubernetes.cri.sandbox-namespace"`
	PodUID                string `mapstructure:"io.kubernetes.cri.sandbox-uid"`
}

type Config struct {
	ZeropodContainerNames []string
	Ports                 []uint16
	ScaleDownDuration     time.Duration
	DisableCheckpointing  bool
	PreDump               bool
	Migrate               []string
	LiveMigrate           string
	ContainerName         string
	ContainerType         string
	PodName               string
	PodNamespace          string
	PodUID                string
	ContainerdNamespace   string
	spec                  *specs.Spec
}

// NewConfig uses the annotations from the container spec to create a new
// typed ZeropodConfig config.
func NewConfig(ctx context.Context, spec *specs.Spec) (*Config, error) {
	cfg := &annotationConfig{}
	if err := mapstructure.Decode(spec.Annotations, cfg); err != nil {
		return nil, err
	}

	var err error
	var containerPorts []uint16
	if len(cfg.PortMap) != 0 {
		for _, mapping := range strings.Split(cfg.PortMap, mappingDelim) {
			namePorts := strings.Split(mapping, mapDelim)
			if len(namePorts) != 2 {
				return nil, fmt.Errorf("invalid port map, the format needs to be name=port")
			}

			name, ports := namePorts[0], namePorts[1]
			if name != cfg.ContainerName {
				continue
			}

			for _, port := range strings.Split(ports, portsDelim) {
				p, err := strconv.ParseUint(port, 10, 16)
				if err != nil {
					return nil, err
				}
				containerPorts = append(containerPorts, uint16(p))
			}
		}
	}

	dur := defaultScaleDownDuration
	if len(cfg.ScaledownDuration) != 0 {
		dur, err = time.ParseDuration(cfg.ScaledownDuration)
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
		containerNames = strings.Split(cfg.ZeropodContainerNames, containersDelim)
	}

	migrate := []string{}
	if len(cfg.Migrate) != 0 {
		migrate = strings.Split(cfg.Migrate, containersDelim)
	}

	ns, ok := namespaces.Namespace(ctx)
	if !ok {
		ns = defaultContainerdNS
	}

	return &Config{
		Ports:                 containerPorts,
		ScaleDownDuration:     dur,
		DisableCheckpointing:  disableCheckpointing,
		PreDump:               preDump,
		Migrate:               migrate,
		LiveMigrate:           cfg.LiveMigrate,
		ZeropodContainerNames: containerNames,
		ContainerName:         cfg.ContainerName,
		ContainerType:         cfg.ContainerType,
		PodName:               cfg.PodName,
		PodNamespace:          cfg.PodNamespace,
		PodUID:                cfg.PodUID,
		ContainerdNamespace:   ns,
		spec:                  spec,
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

func (cfg Config) migrationEnabled() bool {
	return slices.Contains(cfg.Migrate, cfg.ContainerName)
}

func (cfg Config) LiveMigrationEnabled() bool {
	return cfg.LiveMigrate == cfg.ContainerName
}

func (cfg Config) AnyMigrationEnabled() bool {
	return cfg.migrationEnabled() || cfg.LiveMigrationEnabled()
}
