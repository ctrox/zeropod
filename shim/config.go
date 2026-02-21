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
	DisableProbeDetectAnnotationKey  = "zeropod.ctrox.dev/disable-probe-detection"
	ProbeBufferSizeAnnotationKey     = "zeropod.ctrox.dev/probe-buffer-size"
	ProxyTimeoutAnnotationKey        = "zeropod.ctrox.dev/proxy-timeout"
	ConnectTimeoutAnnotationKey      = "zeropod.ctrox.dev/connect-timeout"
	DisableMigrateDataAnnotationKey  = "zeropod.ctrox.dev/disable-migrate-data"
	CRIContainerNameAnnotation       = "io.kubernetes.cri.container-name"
	CRIContainerTypeAnnotation       = "io.kubernetes.cri.container-type"
	CRIPodNameAnnotation             = "io.kubernetes.cri.sandbox-name"
	CRIPodNamespaceAnnotation        = "io.kubernetes.cri.sandbox-namespace"
	CRIPodUIDAnnotation              = "io.kubernetes.cri.sandbox-uid"

	defaultScaleDownDuration = time.Minute
	defaultProxyTimeout      = time.Second * 5
	defaultConnectTimeout    = time.Second * 5
	containersDelim          = ","
	portsDelim               = containersDelim
	mappingDelim             = ";"
	mapDelim                 = "="
	defaultContainerdNS      = "k8s.io"
)

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
	DisableProbeDetection bool
	ProbeBufferSize       int
	ProxyTimeout          time.Duration
	ConnectTimeout        time.Duration
	DisableMigrateData    bool
	spec                  *specs.Spec
}

// NewConfig uses the annotations from the container spec to create a new
// typed ZeropodConfig config.
func NewConfig(ctx context.Context, spec *specs.Spec) (*Config, error) {
	containerName := spec.Annotations[CRIContainerNameAnnotation]
	containerType := spec.Annotations[CRIContainerTypeAnnotation]

	var err error
	var containerPorts []uint16
	portsMap := spec.Annotations[PortsAnnotationKey]
	if portsMap != "" {
		for mapping := range strings.SplitSeq(portsMap, mappingDelim) {
			namePorts := strings.Split(mapping, mapDelim)
			if len(namePorts) != 2 {
				return nil, fmt.Errorf("invalid port map, the format needs to be name=port")
			}

			name, ports := namePorts[0], namePorts[1]
			if name != containerName {
				continue
			}

			for port := range strings.SplitSeq(ports, portsDelim) {
				p, err := strconv.ParseUint(port, 10, 16)
				if err != nil {
					return nil, err
				}
				containerPorts = append(containerPorts, uint16(p))
			}
		}
	}

	scaleDownDuration := spec.Annotations[ScaleDownDurationAnnotationKey]
	dur := defaultScaleDownDuration
	if scaleDownDuration != "" {
		dur, err = time.ParseDuration(scaleDownDuration)
		if err != nil {
			return nil, err
		}
	}

	disableCheckpointValue := spec.Annotations[DisableCheckpoiningAnnotationKey]
	disableCheckpointing := false
	if disableCheckpointValue != "" {
		disableCheckpointing, err = strconv.ParseBool(disableCheckpointValue)
		if err != nil {
			return nil, err
		}
	}

	preDump := false
	preDumpValue := spec.Annotations[PreDumpAnnotationKey]
	if preDumpValue != "" {
		preDump, err = strconv.ParseBool(preDumpValue)
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
	containerNamesValue := spec.Annotations[ContainerNamesAnnotationKey]
	if containerNamesValue != "" {
		containerNames = strings.Split(containerNamesValue, containersDelim)
	}

	migrate := []string{}
	migrateValue := spec.Annotations[MigrateAnnotationKey]
	if migrateValue != "" {
		migrate = strings.Split(migrateValue, containersDelim)
	}

	ns, ok := namespaces.Namespace(ctx)
	if !ok {
		ns = defaultContainerdNS
	}

	disableProbeDetectionValue := spec.Annotations[DisableProbeDetectAnnotationKey]
	disableProbeDetection := false
	if disableProbeDetectionValue != "" {
		disableProbeDetection, err = strconv.ParseBool(disableProbeDetectionValue)
		if err != nil {
			return nil, err
		}
	}

	probeBufferSize := defaultProbeBufferSize
	probeBufferSizeValue := spec.Annotations[ProbeBufferSizeAnnotationKey]
	if probeBufferSizeValue != "" {
		probeBufferSize, err = strconv.Atoi(probeBufferSizeValue)
		if err != nil {
			return nil, err
		}
	}

	proxyTimeout := defaultProxyTimeout
	proxyTimeoutValue := spec.Annotations[ProxyTimeoutAnnotationKey]
	if proxyTimeoutValue != "" {
		proxyTimeout, err = time.ParseDuration(proxyTimeoutValue)
		if err != nil {
			return nil, err
		}
	}

	connectTimeout := defaultConnectTimeout
	connectTimeoutValue := spec.Annotations[ConnectTimeoutAnnotationKey]
	if connectTimeoutValue != "" {
		connectTimeout, err = time.ParseDuration(connectTimeoutValue)
		if err != nil {
			return nil, err
		}
	}

	disableMigrateDataValue := spec.Annotations[DisableMigrateDataAnnotationKey]
	disableMigrateData := false
	if disableMigrateDataValue != "" {
		disableMigrateData, err = strconv.ParseBool(disableMigrateDataValue)
		if err != nil {
			return nil, err
		}
	}

	return &Config{
		Ports:                 containerPorts,
		ScaleDownDuration:     dur,
		DisableCheckpointing:  disableCheckpointing,
		PreDump:               preDump,
		Migrate:               migrate,
		LiveMigrate:           spec.Annotations[LiveMigrateAnnotationKey],
		ZeropodContainerNames: containerNames,
		ContainerName:         containerName,
		ContainerType:         containerType,
		PodName:               spec.Annotations[CRIPodNameAnnotation],
		PodNamespace:          spec.Annotations[CRIPodNamespaceAnnotation],
		PodUID:                spec.Annotations[CRIPodUIDAnnotation],
		ContainerdNamespace:   ns,
		DisableProbeDetection: disableProbeDetection,
		ProbeBufferSize:       probeBufferSize,
		ProxyTimeout:          proxyTimeout,
		ConnectTimeout:        connectTimeout,
		DisableMigrateData:    disableMigrateData,
		spec:                  spec,
	}, nil
}

func (cfg Config) IsZeropodContainer() bool {
	if slices.Contains(cfg.ZeropodContainerNames, cfg.ContainerName) {
		return true
	}

	// if there is none specified, every one of them is considered.
	return len(cfg.ZeropodContainerNames) == 0
}

func (cfg Config) migrationEnabled() bool {
	return slices.Contains(cfg.Migrate, cfg.ContainerName) && !cfg.DisableCheckpointing
}

func (cfg Config) LiveMigrationEnabled() bool {
	return cfg.LiveMigrate == cfg.ContainerName
}

func (cfg Config) AnyMigrationEnabled() bool {
	return cfg.migrationEnabled() || cfg.LiveMigrationEnabled()
}
