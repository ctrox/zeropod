package main

import (
	"context"
	"io"
	"path/filepath"

	"github.com/containerd/containerd/api/types"
	shimbinary "github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/manager"
	"github.com/containerd/containerd/v2/pkg/shim"
	zshim "github.com/ctrox/zeropod/shim"
	_ "github.com/ctrox/zeropod/shim/task/plugin"
)

// compatManager is a wrapper around [shim.Manager] that allows us to control
// the task API version. This makes it possible to use the containerd v2 shim
// with containerd 1.7.
type compatManager struct {
	mgr shim.Manager
}

func (cm compatManager) Name() string {
	return cm.mgr.Name()
}

func (cm compatManager) Start(ctx context.Context, id string, opts shim.StartOpts) (shim.BootstrapParams, error) {
	params, err := cm.mgr.Start(ctx, id, opts)
	if err != nil {
		return params, err
	}
	// TODO: would be nice to detect the containerd version and set 3 for 2.0+.
	// So far it looks like this is not possible. Since containerd v2 works with
	// task v2 right now, this is not a big issue.
	params.Version = 2
	path, err := filepath.Abs("address")
	if err != nil {
		return params, err
	}
	if err := shimbinary.WriteAddress(path, params.Address); err != nil {
		return params, err
	}
	return params, err
}

func (cm compatManager) Stop(ctx context.Context, id string) (shim.StopStatus, error) {
	return cm.mgr.Stop(ctx, id)
}

func (cm compatManager) Info(ctx context.Context, optionsR io.Reader) (*types.RuntimeInfo, error) {
	info, err := cm.mgr.Info(ctx, optionsR)
	return info, err
}

func newCompatManager() shim.Manager {
	return &compatManager{mgr: manager.NewShimManager(zshim.RuntimeName)}
}

func main() {
	shim.Run(context.Background(), newCompatManager())
}
