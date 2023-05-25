package main

import (
	"context"

	"github.com/containerd/containerd/pkg/seed"
	"github.com/containerd/containerd/runtime/v2/runc/manager"
	"github.com/containerd/containerd/runtime/v2/shim"
	_ "github.com/ctrox/zeropod/runc/task/plugin"
	"github.com/ctrox/zeropod/zeropod"
)

func shimConfig(config *shim.Config) {}

func init() {
	seed.WithTimeAndRand()
}

func main() {
	// shim.Run(runc.RuntimeName, v2.New, shimConfig)
	shim.RunManager(context.Background(), manager.NewShimManager(zeropod.RuntimeName))
}
