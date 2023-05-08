package runc

import (
	"github.com/containerd/containerd/pkg/process"
)

const RuntimeName = "io.containerd.zeropod.v2"

func (c *Container) SetMainProcess(p process.Process) {
	c.process = p
}
