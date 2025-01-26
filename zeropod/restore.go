package zeropod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/namespaces"
	crio "github.com/containerd/containerd/pkg/cri/io"
	cioutil "github.com/containerd/containerd/pkg/ioutil"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/stdio"
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containerd/containerd/runtime/v2/runc/options"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	v1 "github.com/ctrox/zeropod/api/node/v1"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrAlreadyRestored      = errors.New("container is already restored")
	ErrRestoreRequestFailed = errors.New("restore request failed")
)

func (c *Container) Restore(ctx context.Context) (*runc.Container, process.Process, error) {
	c.checkpointRestore.Lock()
	defer c.checkpointRestore.Unlock()
	if !c.ScaledDown() {
		return nil, nil, ErrAlreadyRestored
	}

	beforeRestore := time.Now()
	go func() {
		// as soon as we checkpoint the container, the log pipe is closed. As
		// we currently have no way to instruct containerd to restore the logs
		// and pipe it again, we do it manually.
		if err := c.restoreLoggers(c.ID(), c.initialProcess.Stdio()); err != nil {
			log.G(ctx).Errorf("error restoring loggers: %s", err)
		}
	}()

	createReq := &task.CreateTaskRequest{
		ID:               c.ID(),
		Bundle:           c.Bundle,
		Terminal:         false,
		Stdin:            c.initialProcess.Stdio().Stdin,
		Stdout:           c.initialProcess.Stdio().Stdout,
		Stderr:           c.initialProcess.Stdio().Stderr,
		ParentCheckpoint: "",
		Checkpoint:       containerDir(c.Bundle),
	}

	if c.cfg.DisableCheckpointing {
		createReq.Checkpoint = ""
	}

	container, err := runc.NewContainer(namespaces.WithNamespace(ctx, c.cfg.ContainerdNamespace), c.platform, createReq)
	if err != nil {
		return nil, nil, err
	}
	// it's important to restore the cgroup as NewContainer won't set it as
	// the process is not yet restored.
	container.CgroupSet(c.cgroup)

	var handleStarted HandleStartedFunc
	if c.preRestore != nil {
		handleStarted = c.preRestore()
	}

	p, err := container.Process("")
	if err != nil {
		return nil, nil, err
	}
	log.G(ctx).Info("restore: process created")

	if err := p.Start(ctx); err != nil {
		b, err := os.ReadFile(filepath.Join(container.Bundle, "work", "restore.log"))
		if err != nil {
			log.G(ctx).Errorf("error reading restore.log: %s", err)
		}
		log.G(ctx).Errorf("restore.log: %s", b)

		return nil, nil, fmt.Errorf("start failed during restore: %w", err)
	}
	restoreDuration.With(c.labels()).Observe(time.Since(beforeRestore).Seconds())

	c.Container = container
	c.process = p
	c.SetScaledDown(false)

	if c.postRestore != nil {
		c.postRestore(container, handleStarted)
	}

	// process is running again, we don't need to redirect traffic anymore
	if err := c.activator.DisableRedirects(); err != nil {
		return nil, nil, fmt.Errorf("could not disable redirects: %w", err)
	}

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
	// from github.com/containerd/containerd/pkg/cri/config
	const maxContainerLogLineSize = 16 * 1024

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
		stdout, stdoutCh = crio.NewCRILogger(logPath, wc, crio.Stdout, maxContainerLogLineSize)
		// Only redirect stderr when there is no tty.
		if !tty {
			stderr, stderrCh = crio.NewCRILogger(logPath, wc, crio.Stderr, maxContainerLogLineSize)
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

// CreateLazyRestore requests a restore request on the node. If a matching
// migration is found, it sets the Checkpoint path in the CreateTaskRequest.
func CreateLazyRestore(ctx context.Context, r *task.CreateTaskRequest, cfg *Config) error {
	conn, err := net.Dial("unix", v1.SocketPath)
	if err != nil {
		return fmt.Errorf("dialing node service: %w", err)
	}
	log.G(ctx).Infof("creating restore request for container: %s", cfg.ContainerName)

	restoreReq := &v1.RestoreRequest{
		PodInfo: &v1.PodInfo{
			Name:          cfg.PodName,
			Namespace:     cfg.PodNamespace,
			ContainerName: cfg.ContainerName,
		},
	}
	nodeClient := v1.NewNodeClient(ttrpc.NewClient(conn))
	defer conn.Close()
	resp, err := nodeClient.Restore(ctx, restoreReq)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRestoreRequestFailed, err)
	}

	log.G(ctx).Infof("restore response: %v", resp.MigrationInfo)
	// TODO: validate that path is valid and contains image
	r.Checkpoint = v1.SnapshotPath(resp.MigrationInfo.ImageId)

	// wait for the lazy pages socket file to exist to ensure the pages
	// server is up and running.
	if err := waitForLazyPagesSocket(ctx, r.Checkpoint, time.Second); err != nil {
		log.G(ctx).Errorf("aborting restore: %s", err)
		r.Checkpoint = ""
		return nil
	}

	if err := setCriuWorkPath(r, r.Checkpoint); err != nil {
		return err
	}

	log.G(ctx).Infof("setting checkpoint dir for restore: %s", v1.SnapshotPath(resp.MigrationInfo.ImageId))

	return nil
}

func FinishRestore(ctx context.Context, cfg *Config) error {
	conn, err := net.Dial("unix", v1.SocketPath)
	if err != nil {
		return fmt.Errorf("dialing node service: %w", err)
	}
	defer conn.Close()

	restoreReq := &v1.RestoreRequest{
		PodInfo: &v1.PodInfo{
			Name:          cfg.PodName,
			Namespace:     cfg.PodNamespace,
			ContainerName: cfg.ContainerName,
		},
		MigrationInfo: &v1.MigrationInfo{RestoredAt: timestamppb.Now()},
	}
	nodeClient := v1.NewNodeClient(ttrpc.NewClient(conn))
	if _, err := nodeClient.FinishRestore(ctx, restoreReq); err != nil {
		return err
	}

	return nil
}

// waitForLazyPagesSocket waits until the lazy-pages.socket file exists in the
// supplied checkpointPath.
func waitForLazyPagesSocket(ctx context.Context, checkpointPath string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for lazy-pages.socket")
		default:
			log.G(ctx).Info("waiting for lazy-pages.socket")
			_, err := os.Stat(filepath.Join(checkpointPath, "lazy-pages.socket"))
			if err == nil {
				return nil
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func setCriuWorkPath(r *task.CreateTaskRequest, path string) error {
	m := new(options.Options)
	if err := r.Options.UnmarshalTo(m); err != nil {
		return err
	}
	m.CriuWorkPath = path
	any, err := anypb.New(m)
	if err != nil {
		return err
	}
	r.Options = any
	return nil
}
