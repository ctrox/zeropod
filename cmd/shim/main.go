package main

import (
	"context"

	"github.com/containerd/containerd/runtime/v2/runc/manager"
	"github.com/containerd/containerd/runtime/v2/shim"
	zshim "github.com/ctrox/zeropod/shim"
	_ "github.com/ctrox/zeropod/shim/task/plugin"
)

func main() {
	shim.RunManager(context.Background(), manager.NewShimManager(zshim.RuntimeName))
}
