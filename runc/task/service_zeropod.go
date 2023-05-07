package task

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ctrox/zeropod/activator"
	"github.com/ctrox/zeropod/process"
	"github.com/ctrox/zeropod/runc"

	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/stdio"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
)

// StartZeropod starts a zeropod process
func (s *service) StartZeropod(ctx context.Context, r *taskAPI.StartRequest) error {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return err
	}

	spec, err := runc.GetSpec(container.Bundle)
	if err != nil {
		return err
	}

	// switch to network ns of container and start our activator listener
	netNSPath, err := runc.GetNetworkNS(spec)
	if err != nil {
		return err
	}

	// create a new context in order to not run into deadline of parent context
	ctx = log.WithLogger(context.Background(), log.G(ctx).WithField("runtime", runc.RuntimeName))

	cfg, err := NewConfig(spec)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("starting activator with config: %v", cfg)

	srv, err := activator.NewServer(ctx, cfg.Port, netNSPath)
	if err != nil {
		return err
	}

	s.shutdown.RegisterCallback(func(ctx context.Context) error {
		// stop server on shutdown
		srv.Stop(ctx)
		return nil
	})

	if err := srv.Start(ctx, func() (*runc.Container, process.Process, error) {
		log.G(ctx).Printf("got a request")

		// hold the send lock so that the start events are sent before any exit events in the error case
		s.eventSendMu.Lock()

		p, err := s.restore(ctx, container)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
		p.SetScaledDown(false)
		log.G(ctx).Printf("restored process: %d", p.Pid())

		s.send(&eventstypes.TaskStart{
			ContainerID: container.ID,
			Pid:         uint32(p.Pid()),
		})

		s.eventSendMu.Unlock()

		// before returning we set the net ns again as it might have changed
		// in the meantime. (not sure why that happens though)
		return container, p, nil
	}, func(container *runc.Container, p process.Process) error {
		time.Sleep(cfg.ScaleDownDuration)
		log.G(ctx).Info("scaling down after scale down duration is up")
		return s.scaleDown(ctx, r, container, p)
	}); err != nil {
		log.G(ctx).Errorf("failed to start server: %s", err)
		return err
	}

	log.G(ctx).Printf("activator started")
	return nil
}

func (s *service) scaleDown(ctx context.Context, r *taskAPI.StartRequest, container *runc.Container, p process.Process) error {
	snapshotDir := snapshotDir(container.Bundle)

	if err := os.RemoveAll(snapshotDir); err != nil {
		return fmt.Errorf("unable to prepare snapshot dir: %w", err)
	}

	workDir := path.Join(snapshotDir, "work")
	log.G(ctx).Infof("checkpointing process %d of container to %s", p.Pid(), snapshotDir)
	s.stdio = p.Stdio()

	p.SetScaledDown(true)
	// after checkpointing criu locks the network until the process is
	// restored by inserting some iptables rules. We don't want that since our
	// activator needs to be able to accept connections while the process is
	// frozen. As a workaround for the time being we patched criu to just not
	// insert these iptables rules.
	if err := p.(*process.Init).Checkpoint(ctx, &process.CheckpointConfig{
		Path:                     containerDir(container.Bundle),
		WorkDir:                  workDir,
		Exit:                     true,
		AllowOpenTCP:             true,
		AllowExternalUnixSockets: true,
		AllowTerminal:            false,
		FileLocks:                false,
		EmptyNamespaces:          []string{},
	}); err != nil {
		p.SetScaledDown(false)

		log.G(ctx).Errorf("error checkpointing container: %s", err)
		b, err := os.ReadFile(path.Join(workDir, "dump.log"))
		if err != nil {
			log.G(ctx).Errorf("error reading dump.log: %s", err)
		}

		log.G(ctx).Errorf("dump.log: %s", b)

		return err
	}

	s.send(&eventstypes.TaskCheckpointed{
		ContainerID: container.ID,
	})

	log.G(ctx).Info("starting zeropod")

	if err := s.StartZeropod(ctx, r); err != nil {
		log.G(ctx).Errorf("unable to start zeropod: %s", err)
		return err
	}

	return nil
}

func (s *service) restore(ctx context.Context, container *runc.Container) (process.Process, error) {
	// generate a random container ID. TODO: does this matter? This seems to
	// work well enough but not sure what else depends on the container ID.
	container.ID = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano()))))

	runtime := process.NewRunc("", container.Bundle, "k8s", "", "", false)

	log.G(ctx).Infof("stdout %s", s.stdio.Stdout)

	// TODO: we should somehow reuse the original stdio. For now we just
	// create a file for stdout and stderr.
	s.stdio.Stdout = strings.TrimPrefix(s.stdio.Stdout, "file://")
	s.stdio.Stdout = strings.TrimSuffix(s.stdio.Stdout, "-1")
	s.stdio.Stderr = strings.TrimPrefix(s.stdio.Stdout, "file://")
	s.stdio.Stderr = strings.TrimSuffix(s.stdio.Stdout, "-1")

	p := process.New(container.ID, runtime, stdio.Stdio{
		Stdout: "file://" + s.stdio.Stdout + "-1",
		Stderr: "file://" + s.stdio.Stderr + "-1",
	})
	p.Bundle = container.Bundle
	p.Platform = s.platform
	p.WorkDir = filepath.Join(container.Bundle, "work")

	if p.CriuWorkPath == "" {
		// if criu work path not set, use container WorkDir
		p.CriuWorkPath = p.WorkDir
	}

	log.G(ctx).Infof("restoring %s", container.ID)

	if err := p.Create(ctx, &process.CreateConfig{
		ID:         container.ID,
		Bundle:     container.Bundle,
		Checkpoint: containerDir(container.Bundle),
	}); err != nil {
		return nil, fmt.Errorf("creation failed during restore: %w", err)
	}

	log.G(ctx).Info("restore: process created")

	if err := p.Start(ctx); err != nil {
		return nil, fmt.Errorf("start failed during restore: %w", err)
	}

	s.send(&eventstypes.TaskResumed{
		ContainerID: container.ID,
	})

	return p, nil
}

func snapshotDir(bundle string) string {
	return path.Join(bundle, "snapshots")
}

func containerDir(bundle string) string {
	return path.Join(snapshotDir(bundle), "container")
}
