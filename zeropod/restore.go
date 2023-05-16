package zeropod

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/cri/config"
	crio "github.com/containerd/containerd/pkg/cri/io"
	"github.com/containerd/containerd/pkg/cri/util"
	cioutil "github.com/containerd/containerd/pkg/ioutil"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/stdio"
	"github.com/ctrox/zeropod/runc"
)

func (s *Container) Restore(ctx context.Context, container *runc.Container) (process.Process, error) {
	// generate a new container ID as sometimes (probably some race condition)
	// we get a "container with given ID already exists" from runc.
	container.ID = util.GenerateID()
	runtime := process.NewRunc("", container.Bundle, "k8s", "", "", false)

	go func() {
		// as soon as we checkpoint the container, the log pipe is closed. As
		// we currently have no way to instruct containerd to restore the logs
		// and pipe it again, we do it manually.
		if err := s.restoreLoggers(container.ID, s.initialProcess.Stdio()); err != nil {
			log.G(ctx).Errorf("error restoring loggers: %s", err)
		}
	}()

	p := process.New(container.ID, runtime, stdio.Stdio{
		Stdout: s.initialProcess.Stdio().Stdout,
		Stderr: s.initialProcess.Stdio().Stderr,
	})
	p.Bundle = container.Bundle
	p.Platform = s.platform
	p.WorkDir = filepath.Join(container.Bundle, "work")

	if p.CriuWorkPath == "" {
		// if criu work path not set, use container WorkDir
		p.CriuWorkPath = p.WorkDir
	}

	createConfig := &process.CreateConfig{
		ID:     container.ID,
		Bundle: container.Bundle,
	}

	if s.cfg.Stateful {
		log.G(ctx).Infof("container %s is stateful, restoring from checkpoint", container.ID)
		createConfig.Checkpoint = containerDir(container.Bundle)
	} else {
		log.G(ctx).Infof("restoring %s by starting the process again", container.ID)
	}

	if err := p.Create(ctx, createConfig); err != nil {
		return nil, fmt.Errorf("creation failed during restore: %w", err)
	}

	log.G(ctx).Info("restore: process created")

	if err := p.Start(ctx); err != nil {
		return nil, fmt.Errorf("start failed during restore: %w", err)
	}

	s.id = container.ID
	container.SetMainProcess(p)

	return p, nil
}

// restoreLoggers creates the appropriate fifos and pipes the logs to the
// container log at s.logPath. It blocks until the logs are closed. This has
// been adapted from internal containerd code and the logging setup should be
// pretty much the same.
func (s *Container) restoreLoggers(id string, stdio stdio.Stdio) error {
	fifos := cio.NewFIFOSet(cio.Config{
		Stdin:    "",
		Stdout:   stdio.Stdout,
		Stderr:   stdio.Stderr,
		Terminal: false,
	}, func() error { return nil })

	stdoutWC, stderrWC, err := createContainerLoggers(s.context, s.logPath, false)
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
