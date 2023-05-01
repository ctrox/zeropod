package runc

import (
	"context"
	"fmt"
	"os"

	"github.com/ctrox/zeropod/process"

	"github.com/containerd/containerd/runtime/v2/task"
)

const RuntimeName = "io.containerd.zeropod.v2"

// Run a container process
func (c *Container) Run(ctx context.Context, r *task.StartRequest, f *os.File) (process.Process, error) {
	p, err := c.Process(r.ExecID)
	if err != nil {
		return nil, err
	}

	initProcess, ok := p.(*process.Init)
	if !ok {
		return nil, fmt.Errorf("process is not of type process.Init: %T", p)
	}

	return initProcess, initProcess.Run(ctx, &process.CreateConfig{
		ID:         r.ID,
		Bundle:     initProcess.Bundle,
		Terminal:   p.Stdio().Terminal,
		Stdin:      p.Stdio().Stdin,
		Stdout:     p.Stdio().Stdout,
		Stderr:     p.Stdio().Stderr,
		ExtraFiles: []*os.File{f},
	})
}
