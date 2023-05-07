package task

import (
	"strconv"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/opencontainers/runtime-spec/specs-go"
)

type annotationConfig struct {
	Port              string `mapstructure:"dev.ctrox.zeropod/port"`
	ScaleDownDuration string `mapstructure:"dev.ctrox.zeropod/scaledownduration"`
}

type ZeropodConfig struct {
	Port              uint16
	ScaleDownDuration time.Duration
}

// NewConfig uses the annotations from the container spec to create a new
// typed ZeropodConfig config.
func NewConfig(spec *specs.Spec) (*ZeropodConfig, error) {
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

	return &ZeropodConfig{
		Port:              uint16(port),
		ScaleDownDuration: dur,
	}, nil
}
