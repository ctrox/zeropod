package zeropod

import (
	"context"
	"testing"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewConfig(t *testing.T) {
	tests := map[string]struct {
		annotations map[string]string
		assertCfg   func(t *testing.T, cfg *Config)
	}{
		"ports": {
			annotations: map[string]string{
				CRIContainerNameAnnotation: "container1",
				PortsAnnotationKey:         "container0=1234;container1=80,81;container2=8080",
			},
			assertCfg: func(t *testing.T, cfg *Config) {
				assert.Equal(t, []uint16{80, 81}, cfg.Ports)
			},
		},
		"container names": {
			annotations: map[string]string{
				CRIContainerNameAnnotation:  "container1",
				ContainerNamesAnnotationKey: "container0,container1,container2",
			},
			assertCfg: func(t *testing.T, cfg *Config) {
				assert.Equal(t, "container1", cfg.ContainerName)
				assert.Equal(t, []string{"container0", "container1", "container2"},
					cfg.ZeropodContainerNames)
			},
		},
		"scaledown duration": {
			annotations: map[string]string{
				ScaleDownDurationAnnotationKey: "5m",
			},
			assertCfg: func(t *testing.T, cfg *Config) {
				assert.Equal(t, time.Minute*5, cfg.ScaleDownDuration)
			},
		},
		"disable checkpointing": {
			annotations: map[string]string{
				DisableCheckpoiningAnnotationKey: "true",
			},
			assertCfg: func(t *testing.T, cfg *Config) {
				assert.True(t, cfg.DisableCheckpointing)
			},
		},
		"enable checkpointing": {
			annotations: map[string]string{
				DisableCheckpoiningAnnotationKey: "false",
			},
			assertCfg: func(t *testing.T, cfg *Config) {
				assert.False(t, cfg.DisableCheckpointing)
			},
		},
		"predump": {
			annotations: map[string]string{
				PreDumpAnnotationKey: "true",
			},
			assertCfg: func(t *testing.T, cfg *Config) {
				assert.True(t, cfg.PreDump)
			},
		},
		"disable predump": {
			annotations: map[string]string{
				PreDumpAnnotationKey: "false",
			},
			assertCfg: func(t *testing.T, cfg *Config) {
				assert.False(t, cfg.PreDump)
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg, err := NewConfig(context.Background(), &specs.Spec{
				Annotations: tc.annotations,
			})
			require.NoError(t, err)
			tc.assertCfg(t, cfg)
		})
	}
}
