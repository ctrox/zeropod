package shim

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/runc"
	"github.com/containerd/containerd/v2/pkg/cio"
	cioutil "github.com/containerd/containerd/v2/pkg/ioutil"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	crio "github.com/ctrox/zeropod/shim/io"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrAlreadyRestored      = errors.New("container is already restored")
	ErrRestoreRequestFailed = errors.New("restore request failed")
	ErrRestoreDial          = errors.New("failed to connect to node socket")
)

const lazyCancelTimeout = time.Minute

func (c *Container) Restore(ctx context.Context) (*runc.Container, error) {
	c.checkpointRestore.Lock()
	defer c.checkpointRestore.Unlock()
	if !c.ScaledDown() {
		return nil, ErrAlreadyRestored
	}

	go func() {
		// as soon as we checkpoint the container, the log pipe is closed. As
		// we currently have no way to instruct containerd to restore the logs
		// and pipe it again, we do it manually.
		if err := c.restoreLoggers(
			c.ID(),
			stdio.Stdio{
				Stdin:    c.initialProcess.Stdio().Stdin,
				Stdout:   c.initialProcess.Stdio().Stdout,
				Stderr:   c.initialProcess.Stdio().Stderr,
				Terminal: c.initialProcess.Stdio().Terminal,
			},
		); err != nil {
			log.G(ctx).Errorf("error restoring loggers: %s", err)
		}
	}()

	req := &task.CreateTaskRequest{
		ID:               c.ID(),
		Options:          c.createOpts,
		Bundle:           c.Bundle,
		Terminal:         false,
		Stdin:            c.initialProcess.Stdio().Stdin,
		Stdout:           c.initialProcess.Stdio().Stdout,
		Stderr:           c.initialProcess.Stdio().Stderr,
		ParentCheckpoint: "",
		Checkpoint:       nodev1.SnapshotPath(c.ID()),
	}

	if !c.cfg.LazyRestore {
		return c.restore(ctx, req)
	}

	lazyCtx, lazyCancel := context.WithTimeout(ctx, lazyCancelTimeout)
	if err := c.prepareLazyRestore(lazyCtx, req); err != nil {
		return nil, fmt.Errorf("preparing lazy restore: %w", err)
	}
	cont, err := c.restore(ctx, req)
	if err != nil {
		// only cancel lazy restore on error, a successful restore will happen
		// in the background.
		lazyCancel()
	}
	return cont, err
}

func (c *Container) restore(ctx context.Context, req *task.CreateTaskRequest) (*runc.Container, error) {
	beforeRestore := time.Now()
	if c.cfg.DisableCheckpointing {
		req.Checkpoint = ""
	}

	container, err := runc.NewContainer(namespaces.WithNamespace(ctx, c.cfg.ContainerdNamespace), c.platform, req)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	log.G(ctx).Info("restore: process created")

	if err := p.Start(ctx); err != nil {
		b, logErr := os.ReadFile(filepath.Join(container.Bundle, "work", "restore.log"))
		if logErr != nil {
			log.G(ctx).Errorf("error reading restore.log: %s", logErr)
		} else {
			log.G(ctx).Errorf("restore.log: %s", b)
		}
		return nil, fmt.Errorf("start failed during restore: %w", err)
	}
	c.Container = container
	c.process = p
	c.setPhaseNotify(v1.ContainerPhase_RUNNING, time.Since(beforeRestore))
	log.G(ctx).Printf("restored process: %d in %s", p.Pid(), c.metrics.LastRestoreDuration.AsDuration())

	if c.postRestore != nil {
		c.postRestore(container, handleStarted)
	}

	// process is running again, we don't need to redirect traffic anymore
	if err := c.activator.DisableRedirects(); err != nil {
		return nil, fmt.Errorf("could not disable redirects: %w", err)
	}

	return container, nil
}

func (c *Container) prepareLazyRestore(ctx context.Context, createReq *task.CreateTaskRequest) error {
	beforeLazy := time.Now()
	go func() {
		c.lazyRestore.Lock()
		defer c.lazyRestore.Unlock()
		args := []string{
			"-o", "/dev/stdout", "-v", "lazy-pages", "--images-dir",
			nodev1.SnapshotPath(c.ID()), "--work-dir", nodev1.SnapshotPath(c.ID()),
		}
		cmd := exec.CommandContext(ctx, "criu", args...)
		log.G(ctx).Info("starting lazy-pages daemon", "cmd", cmd.Args)
		execLogger := newExecLogger(ctx, "criu-lazy-pages")
		cmd.Stderr = execLogger
		cmd.Stdout = execLogger
		if err := cmd.Start(); err != nil {
			log.G(ctx).WithError(err).Error("error running lazy-pages daemon")
			return
		}
		cmd.Wait()
		log.G(ctx).Info("lazy-pages server closed")
	}()
	if err := waitForLazyPagesSocket(ctx, nodev1.SnapshotPath(c.ID()), time.Second); err != nil {
		return err
	}
	log.G(ctx).WithField("duration", time.Since(beforeLazy).String()).Debug("lazy-pages daemon started")

	if err := setCriuWorkPath(createReq, createReq.Checkpoint); err != nil {
		return err
	}
	return nil
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

// MigrationRestore requests a restore from the node. If a matching migration is
// found, it sets the Checkpoint path in the CreateTaskRequest.
func MigrationRestore(ctx context.Context, r *task.CreateTaskRequest, cfg *Config) (skipStart bool, err error) {
	conn, err := net.Dial("unix", nodev1.SocketPath)
	if err != nil {
		return false, fmt.Errorf("%w: dialing node service: %w", ErrRestoreDial, err)
	}
	log.G(ctx).Infof("creating restore request for container: %s", cfg.ContainerName)

	restoreReq := &nodev1.RestoreRequest{
		MigrationInfo: &nodev1.MigrationInfo{ImageId: r.ID},
		PodInfo: &nodev1.PodInfo{
			Name:          cfg.PodName,
			Namespace:     cfg.PodNamespace,
			ContainerName: cfg.ContainerName,
		},
	}
	nodeClient := nodev1.NewNodeClient(ttrpc.NewClient(conn))
	defer conn.Close()
	resp, err := nodeClient.Restore(ctx, restoreReq)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrRestoreRequestFailed, err)
	}
	if len(cfg.Ports) == 0 {
		for _, p := range resp.MigrationInfo.Ports {
			cfg.Ports = append(cfg.Ports, uint16(p))
		}
	}

	log.G(ctx).Infof("restore response: %v", resp.MigrationInfo)

	// TODO: validate that path is valid and contains image
	r.Checkpoint = nodev1.SnapshotPath(resp.MigrationInfo.ImageId)
	log.G(ctx).Infof("setting checkpoint dir for restore: %s", nodev1.SnapshotPath(resp.MigrationInfo.ImageId))

	// we set the criu work path for the live migration to work (the lazy pages
	// socket needs to be there) and also so the restore stats are stored in the
	// image directory.
	if err := setCriuWorkPath(r, r.Checkpoint); err != nil {
		return false, err
	}

	if !resp.MigrationInfo.LiveMigration {
		skipStart = true
		return
	}

	// wait for the lazy pages socket file to exist to ensure the pages
	// server is up and running.
	if err := waitForLazyPagesSocket(ctx, r.Checkpoint, time.Second); err != nil {
		log.G(ctx).Errorf("aborting restore: %s", err)
		r.Checkpoint = ""
		return false, nil
	}

	return false, nil
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

func FinishRestore(ctx context.Context, id string, cfg *Config, startTime time.Time) error {
	conn, err := net.Dial("unix", nodev1.SocketPath)
	if err != nil {
		return fmt.Errorf("dialing node service: %w", err)
	}
	defer conn.Close()

	restoreReq := &nodev1.RestoreRequest{
		PodInfo: &nodev1.PodInfo{
			Name:          cfg.PodName,
			Namespace:     cfg.PodNamespace,
			ContainerName: cfg.ContainerName,
		},
		MigrationInfo: &nodev1.MigrationInfo{
			ImageId:      id,
			RestoreStart: timestamppb.New(startTime),
			RestoreEnd:   timestamppb.Now(),
		},
	}
	nodeClient := nodev1.NewNodeClient(ttrpc.NewClient(conn))
	if _, err := nodeClient.FinishRestore(ctx, restoreReq); err != nil {
		return err
	}

	return nil
}

type execLogger struct {
	name string
	log  *log.Entry
}

func newExecLogger(ctx context.Context, name string) *execLogger {
	return &execLogger{
		log:  log.G(ctx),
		name: name,
	}
}

func (el execLogger) Write(p []byte) (n int, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(p))
	for scanner.Scan() {
		el.log.WithField("exec", el.name).Debug(scanner.Text())
	}
	return len(p), nil
}
