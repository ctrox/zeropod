package main

import (
	"context"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/manager"
	"github.com/containerd/containerd/v2/pkg/seed"
	"github.com/containerd/containerd/v2/runtime/v2/shim"
	_ "github.com/ctrox/zeropod/runc/task/plugin"
	"github.com/ctrox/zeropod/zeropod"
)

func init() {
	seed.WithTimeAndRand()
}

func main() {
	shim.Run(context.Background(), manager.NewShimManager(zeropod.RuntimeName))
}
