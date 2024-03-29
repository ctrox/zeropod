package main

import (
	"context"

	"github.com/containerd/containerd/runtime/v2/runc/manager"
	"github.com/containerd/containerd/runtime/v2/shim"
	_ "github.com/ctrox/zeropod/runc/task/plugin"
	"github.com/ctrox/zeropod/zeropod"
)

func main() {
	shim.RunManager(context.Background(), manager.NewShimManager(zeropod.RuntimeName))
}
