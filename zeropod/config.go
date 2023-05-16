package zeropod

import (
	"strconv"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/opencontainers/runtime-spec/specs-go"
)

type annotationConfig struct {
	Port                 string `mapstructure:"zeropod.ctrox.dev/port"`
	ScaleDownDuration    string `mapstructure:"zeropod.ctrox.dev/scaledownduration"`
	Stateful             string `mapstructure:"zeropod.ctrox.dev/stateful"`
	ZeropodContainerName string `mapstructure:"zeropod.ctrox.dev/container-name"`
	ContainerName        string `mapstructure:"io.kubernetes.cri.container-name"`
	ContainerType        string `mapstructure:"io.kubernetes.cri.container-type"`
}

type Config struct {
	Port                 uint16
	ScaleDownDuration    time.Duration
	Stateful             bool
	ZeropodContainerName string
	ContainerName        string
	ContainerType        string
}

// NewConfig uses the annotations from the container spec to create a new
// typed ZeropodConfig config.
func NewConfig(spec *specs.Spec) (*Config, error) {
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

	return &Config{
		Port:                 uint16(port),
		ScaleDownDuration:    dur,
		Stateful:             stateful,
		ZeropodContainerName: cfg.ZeropodContainerName,
		ContainerName:        cfg.ContainerName,
		ContainerType:        cfg.ContainerType,
	}, nil
}
