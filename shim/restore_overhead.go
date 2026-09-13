package shim

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/log"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
)

type restoreOverhead struct {
	memoryLimit               int64
	initialMemoryLimit        int64
	initialSandboxMemoryLimit int64
	containerGroup            *cgroup2.Manager
	sandboxGroup              *cgroup2.Manager
}

func newRestoreOverhead(ctx context.Context, cfg *v1.Config, pid int) restoreOverhead {
	if cfg.RestoreOverhead <= 1 {
		return restoreOverhead{}
	}
	if cfg.Spec.Linux == nil || cfg.Spec.Linux.Resources == nil ||
		cfg.Spec.Linux.Resources.Memory == nil || cfg.Spec.Linux.Resources.Memory.Limit == nil {
		return restoreOverhead{}
	}

	containerGroup, err := loadCgroupManager(pid, false)
	if err != nil {
		log.G(ctx).WithError(err).Error("loading container systemd group")
		return restoreOverhead{}
	}

	sandboxGroup, err := loadCgroupManager(pid, true)
	if err != nil {
		log.G(ctx).WithError(err).Error("loading sandbox systemd group")
		return restoreOverhead{}
	}
	m, err := sandboxGroup.Stat()
	if err != nil {
		return restoreOverhead{}
	}

	return restoreOverhead{
		memoryLimit:               int64(float64(*cfg.Spec.Linux.Resources.Memory.Limit) * cfg.RestoreOverhead),
		initialMemoryLimit:        *cfg.Spec.Linux.Resources.Memory.Limit,
		initialSandboxMemoryLimit: int64(m.Memory.UsageLimit),
		containerGroup:            containerGroup,
		sandboxGroup:              sandboxGroup,
	}
}

func (r *restoreOverhead) apply(bundle string, spec *specs.Spec) error {
	if r.containerGroup == nil || r.sandboxGroup == nil {
		return nil
	}

	oldLimit := spec.Linux.Resources.Memory.Limit
	r.initialMemoryLimit = *oldLimit
	newLimit := new(r.memoryLimit)
	spec.Linux.Resources.Memory.Limit = newLimit
	spec.Linux.Resources.Memory.Swap = newLimit
	if err := WriteSpec(spec, bundle); err != nil {
		return fmt.Errorf("updating bundle: %w", err)
	}

	if r.initialSandboxMemoryLimit < *newLimit {
		if err := r.sandboxGroup.Update(&cgroup2.Resources{
			Memory: &cgroup2.Memory{
				Max:  newLimit,
				Swap: newLimit,
			},
		}); err != nil {
			return err
		}
	}

	return nil
}

func (r *restoreOverhead) revert(bundle string, spec *specs.Spec) error {
	if r.containerGroup == nil || r.sandboxGroup == nil {
		return nil
	}

	errs := []error{}
	newLimit := new(r.initialMemoryLimit)
	spec.Linux.Resources.Memory.Limit = newLimit
	spec.Linux.Resources.Memory.Swap = newLimit
	if err := WriteSpec(spec, bundle); err != nil {
		errs = append(errs, fmt.Errorf("updating bundle: %w", err))
	}

	if err := r.sandboxGroup.Update(&cgroup2.Resources{
		Memory: &cgroup2.Memory{
			Max:  &r.initialSandboxMemoryLimit,
			Swap: &r.initialSandboxMemoryLimit,
		},
	}); err != nil {
		errs = append(errs, fmt.Errorf("updating sandbox cgroup: %w", err))
	}

	if err := r.containerGroup.Update(&cgroup2.Resources{
		Memory: &cgroup2.Memory{
			Max:  &r.initialMemoryLimit,
			Swap: &r.initialMemoryLimit,
		},
	}); err != nil {
		errs = append(errs, fmt.Errorf("updating container cgroup: %w", err))
	}

	return errors.Join(errs...)
}

func loadCgroupManager(pid int, sandbox bool) (*cgroup2.Manager, error) {
	path, err := cgroup2.PidGroupPath(pid)
	if err != nil {
		return nil, err
	}
	if sandbox {
		path = filepath.Dir(path)
	}
	return cgroup2.Load(path)
}
