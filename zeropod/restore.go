package zeropod

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/pkg/cri/config"
	crio "github.com/containerd/containerd/pkg/cri/io"
	cioutil "github.com/containerd/containerd/pkg/ioutil"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/stdio"
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containerd/containerd/runtime/v2/task"
)

func (c *Container) Restore(ctx context.Context, container *runc.Container) (*runc.Container, process.Process, error) {
	go func() {
		// as soon as we checkpoint the container, the log pipe is closed. As
		// we currently have no way to instruct containerd to restore the logs
		// and pipe it again, we do it manually.
		if err := c.restoreLoggers(container.ID, c.initialProcess.Stdio()); err != nil {
			log.G(ctx).Errorf("error restoring loggers: %s", err)
		}
	}()

	createReq := &task.CreateTaskRequest{
		ID:               container.ID,
		Bundle:           container.Bundle,
		Terminal:         false,
		Stdin:            c.initialProcess.Stdio().Stdin,
		Stdout:           c.initialProcess.Stdio().Stdout,
		Stderr:           c.initialProcess.Stdio().Stderr,
		ParentCheckpoint: "",
	}

	if c.cfg.Stateful {
		createReq.Checkpoint = containerDir(container.Bundle)
	}

	container, err := runc.NewContainer(namespaces.WithNamespace(ctx, "k8s"), c.platform, createReq)
	if err != nil {
		return nil, nil, err
	}

	p, err := container.Process("")
	if err != nil {
		return nil, nil, err
	}
	log.G(ctx).Info("restore: process created")

	if err := p.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start failed during restore: %w", err)
	}

	c.id = container.ID
	c.process = p

	return container, p, nil
}

// restoreLoggers creates the appropriate fifos and pipes the logs to the
// container log at s.logPath. It blocks until the logs are closed. This has
// been adapted from internal containerd code and the logging setup should be
// pretty much the same.
func (c *Container) restoreLoggers(id string, stdio stdio.Stdio) error {
	fifos := cio.NewFIFOSet(cio.Config{
		Stdin:    "",
		Stdout:   stdio.Stdout,
		Stderr:   stdio.Stderr,
		Terminal: false,
	}, func() error { return nil })

	stdoutWC, stderrWC, err := createContainerLoggers(c.context, c.logPath, false)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if stdoutWC != nil {
				stdoutWC.Close()
			}
			if stderrWC != nil {
				stderrWC.Close()
			}
		}
	}()
	containerIO, err := crio.NewContainerIO(id, crio.WithFIFOs(fifos))
	if err != nil {
		return err
	}
	containerIO.AddOutput("log", stdoutWC, stderrWC)
	containerIO.Pipe()

	return nil
}

func createContainerLoggers(ctx context.Context, logPath string, tty bool) (stdout io.WriteCloser, stderr io.WriteCloser, err error) {
	if logPath != "" {
		// Only generate container log when log path is specified.
		f, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0777)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create and open log file: %w", err)
		}
		defer func() {
			if err != nil {
				f.Close()
			}
		}()
		var stdoutCh, stderrCh <-chan struct{}
		wc := cioutil.NewSerialWriteCloser(f)
		stdout, stdoutCh = crio.NewCRILogger(logPath, wc, crio.Stdout, config.DefaultConfig().MaxContainerLogLineSize)
		// Only redirect stderr when there is no tty.
		if !tty {
			stderr, stderrCh = crio.NewCRILogger(logPath, wc, crio.Stderr, config.DefaultConfig().MaxContainerLogLineSize)
		}
		go func() {
			if stdoutCh != nil {
				<-stdoutCh
			}
			if stderrCh != nil {
				<-stderrCh
			}
			log.G(ctx).Infof("finish redirecting log file %q, closing it", logPath)
			f.Close()
		}()
	} else {
		stdout = crio.NewDiscardLogger()
		stderr = crio.NewDiscardLogger()
	}
	return
}
