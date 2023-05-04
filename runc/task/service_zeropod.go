package task

import (
	"context"
	"fmt"
	"path"
	"path/filepath"

	"github.com/ctrox/zeropod/activator"
	"github.com/ctrox/zeropod/process"
	"github.com/ctrox/zeropod/runc"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netns"

	cgroupsv2 "github.com/containerd/cgroups/v2"
	"github.com/containerd/cgroups/v3/cgroup1"
	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/stdio"
	"github.com/containerd/containerd/pkg/userns"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
)

// StartZeropod starts a zeropod process
func (s *service) StartZeropod(ctx context.Context, r *taskAPI.StartRequest) error {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return err
	}

	if err := s.ensureNetworkNS(ctx, container); err != nil {
		return err
	}

	s.activator.Do(func() {
		// create a new context in order to not run into deadline of parent context
		ctx = log.WithLogger(context.Background(), log.G(ctx).WithField("runtime", runc.RuntimeName))
		log.G(ctx).Printf("starting activator")
		// TODO: extract this port from container
		port := 80
		srv := activator.NewServer(ctx, port)

		s.shutdown.RegisterCallback(func(ctx context.Context) error {
			// stop server on shutdown
			srv.Stop(ctx)
			return nil
		})

		if err := srv.Start(ctx, func() error {
			log.G(ctx).Printf("got a request")

			// hold the send lock so that the start events are sent before any exit events in the error case
			s.eventSendMu.Lock()

			p, err := s.restore(ctx, container)
			if err != nil {
				return fmt.Errorf("error restoring container: %w", err)
			}
			p.SetScaledDown(false)

			switch r.ExecID {
			case "":
				switch cg := container.Cgroup().(type) {
				case cgroup1.Cgroup:
					if err := s.ep.Add(container.ID, cg); err != nil {
						logrus.WithError(err).Error("add cg to OOM monitor")
					}
				case *cgroupsv2.Manager:
					allControllers, err := cg.RootControllers()
					if err != nil {
						logrus.WithError(err).Error("failed to get root controllers")
					} else {
						if err := cg.ToggleControllers(allControllers, cgroupsv2.Enable); err != nil {
							if userns.RunningInUserNS() {
								logrus.WithError(err).Debugf("failed to enable controllers (%v)", allControllers)
							} else {
								logrus.WithError(err).Errorf("failed to enable controllers (%v)", allControllers)
							}
						}
					}
					if err := s.ep.Add(container.ID, cg); err != nil {
						logrus.WithError(err).Error("add cg to OOM monitor")
					}
				}

				s.send(&eventstypes.TaskStart{
					ContainerID: container.ID,
					Pid:         uint32(p.Pid()),
				})
			default:
				s.send(&eventstypes.TaskExecStarted{
					ContainerID: container.ID,
					ExecID:      r.ExecID,
					Pid:         uint32(p.Pid()),
				})
			}
			s.eventSendMu.Unlock()

			return s.ensureNetworkNS(ctx, container)
		}); err != nil {
			log.G(ctx).Fatalf("failed to start server on port %d: %s", port, err)
		}
	})

	log.G(ctx).Printf("activator started")
	return nil
}

func (s *service) ensureNetworkNS(ctx context.Context, container *runc.Container) error {
	// switch to network ns of container and start our activator listener
	netNSPath, err := runc.GetNetworkNS(ctx, container.Bundle)
	if err != nil {
		return err
	}

	ns, err := netns.GetFromPath(netNSPath)
	if err != nil {
		return err
	}

	if err := netns.Set(ns); err != nil {
		return err
	}

	log.G(ctx).Infof("set netns to %s", netNSPath)
	return nil
}

func (s *service) restore(ctx context.Context, container *runc.Container) (process.Process, error) {
	// ns, err := namespaces.NamespaceFromEnv
	// if err != nil {
	// 	return nil, fmt.Errorf("create namespace: %w", err)
	// }

	runtime := process.NewRunc("", container.Bundle, "k8s", "", "", false)
	p := process.New(container.ID, runtime, stdio.Stdio{
		// TODO: figour out stdout/stderr paths for fixing logs after restore
		Stdout: "",
		Stderr: "",
	})
	p.Bundle = container.Bundle
	p.Platform = s.platform
	// p.Rootfs = rootfs
	p.WorkDir = filepath.Join(container.Bundle, "work")
	// p.IoUID = int(options.IoUid)
	// p.IoGID = int(options.IoGid)
	// p.NoPivotRoot = options.NoPivotRoot
	// p.NoNewKeyring = options.NoNewKeyring
	// p.CriuWorkPath = options.CriuWorkPath

	if p.CriuWorkPath == "" {
		// if criu work path not set, use container WorkDir
		p.CriuWorkPath = p.WorkDir
	}

	if err := p.Create(ctx, &process.CreateConfig{
		ID:         container.ID,
		Bundle:     container.Bundle,
		Checkpoint: containerDir(container.Bundle),
	}); err != nil {
		return nil, err
	}

	return p, p.Start(ctx)
}

func snapshotDir(bundle string) string {
	return path.Join(bundle, "snapshots")
}

func containerDir(bundle string) string {
	return path.Join(snapshotDir(bundle), "container")
}
