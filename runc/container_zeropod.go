package runc

import (
	"github.com/ctrox/zeropod/process"
)

const RuntimeName = "io.containerd.zeropod.v2"

func (c *Container) SetMainProcess(p process.Process) {
	c.process = p
}
