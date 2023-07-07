package task

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ctrox/zeropod/zeropod"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/sirupsen/logrus"

	"github.com/containerd/cgroups"
	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/cri/annotations"
	"github.com/containerd/containerd/pkg/oom"
	oomv1 "github.com/containerd/containerd/pkg/oom/v1"
	oomv2 "github.com/containerd/containerd/pkg/oom/v2"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/containerd/sys/reaper"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/ttrpc"
)

var (
	_ = (taskAPI.TaskService)(&wrapper{})
)

func NewZeropodService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TaskService, error) {
	var (
		ep  oom.Watcher
		err error
	)
	if cgroups.Mode() == cgroups.Unified {
		ep, err = oomv2.New(publisher)
	} else {
		ep, err = oomv1.New(publisher)
	}
	if err != nil {
		return nil, err
	}
	go ep.Run(ctx)
	s := &service{
		context:    ctx,
		events:     make(chan interface{}, 128),
		ec:         reaper.Default.Subscribe(),
		ep:         ep,
		shutdown:   sd,
		containers: make(map[string]*runc.Container),
	}
	w := &wrapper{
		service: s,
	}
	go w.processExits()
	runcC.Monitor = reaper.Default
	if err := w.initPlatform(); err != nil {
		return nil, fmt.Errorf("failed to initialized platform behavior: %w", err)
	}
	go w.forward(ctx, publisher)
	sd.RegisterCallback(func(context.Context) error {
		close(w.events)
		return nil
	})

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			return shim.RemoveSocket(address)
		})
	}

	return w, err
}

type wrapper struct {
	*service

	scaledContainer *zeropod.Container
}

func (w *wrapper) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTaskService(server, w)
	return nil
}

func (w *wrapper) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).Infof("start called in zeropod service %s, %s", r.ID, r.ExecID)

	resp, err := w.service.Start(ctx, r)
	if err != nil {
		return nil, err
	}

	container, err := w.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	spec, err := zeropod.GetSpec(container.Bundle)
	if err != nil {
		return nil, err
	}

	cfg, err := zeropod.NewConfig(ctx, spec)
	if err != nil {
		return nil, err
	}

	// if we have a sandbox container, an exec ID is set or the container does
	// not match the configured one we should not do anything further with the
	// container.
	if cfg.ContainerType == annotations.ContainerTypeSandbox ||
		len(r.ExecID) != 0 ||
		cfg.ZeropodContainerName != cfg.ContainerName {
		return resp, nil
	}

	log.G(ctx).Infof("found zeropod container: %s", cfg.ContainerName)

	scaledContainer, err := zeropod.New(w.context, spec, cfg, container, w.platform)
	if err != nil {
		return nil, err
	}

	w.shutdown.RegisterCallback(func(ctx context.Context) error {
		// stop server on shutdown
		scaledContainer.Stop(ctx)
		return nil
	})

	w.scaledContainer = scaledContainer

	if err := w.scaledContainer.ScheduleScaleDown(container); err != nil {
		return nil, err
	}

	return resp, err
}

func (w *wrapper) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	container, err := w.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	w.scaledContainer.CancelScaleDown()

	// restore it for exec in case we are scaled down
	if w.isScaledDownContainer(r.ID) {
		log.G(ctx).Printf("got exec for scaled down container, restoring")
		beforeRestore := time.Now()

		w.scaledContainer.StopActivator(ctx)

		restoredContainer, p, err := w.scaledContainer.Restore(ctx, container)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
		w.AddContainer(restoredContainer)
		w.scaledContainer.SetScaledDown(false)
		log.G(ctx).Printf("restored process for exec: %d in %s", p.Pid(), time.Since(beforeRestore))
	}

	return w.service.Exec(ctx, r)
}

func (w *wrapper) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	if len(r.ExecID) != 0 && r.ID == w.scaledContainer.InitialID() {
		container, err := w.getContainer(r.ID)
		if err != nil {
			return nil, err
		}

		// on delete of an exec container we want to schedule scaling down again.
		if err := w.scaledContainer.ScheduleScaleDown(container); err != nil {
			return nil, err
		}
	}
	return w.service.Delete(ctx, r)
}

func (w *wrapper) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	if len(r.ExecID) == 0 && w.isScaledDownContainer(r.ID) {
		log.G(ctx).Infof("requested scaled down process %d to be killed", w.scaledContainer.Process().Pid())
		w.scaledContainer.Process().SetExited(0)
		w.scaledContainer.InitialProcess().SetExited(0)
		return w.service.Kill(ctx, r)
	}

	if len(r.ExecID) == 0 && w.scaledContainer != nil && w.scaledContainer.InitialID() == r.ID {
		log.G(ctx).Infof("requested initial container %s to be killed", w.scaledContainer.InitialID())
		w.scaledContainer.Stop(ctx)

		if err := w.scaledContainer.Process().Kill(ctx, r.Signal, r.All); err != nil {
			return nil, errdefs.ToGRPC(err)
		}

		w.scaledContainer.InitialProcess().SetExited(0)
	}

	return w.service.Kill(ctx, r)
}

func (w *wrapper) isScaledDownContainer(id string) bool {
	if w.scaledContainer == nil {
		return false
	}

	return id == w.scaledContainer.InitialID() && w.scaledContainer.ScaledDown()
}

func (w *wrapper) processExits() {
	for e := range w.ec {
		w.checkProcesses(e)
	}
}

func (w *wrapper) checkProcesses(e runcC.Exit) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, container := range w.containers {
		if !container.HasPid(e.Pid) {
			continue
		}

		for _, p := range container.All() {
			if p.Pid() != e.Pid {
				continue
			}

			if ip, ok := p.(*process.Init); ok {
				// Ensure all children are killed
				if runc.ShouldKillAllOnExit(w.context, container.Bundle) {
					if err := ip.KillAll(w.context); err != nil {
						logrus.WithError(err).WithField("id", ip.ID()).
							Error("failed to kill init's children")
					}
				}
			}

			if w.scaledContainer != nil {
				if w.scaledContainer.ScaledDown() && container.ID == w.scaledContainer.ID() {
					log.G(w.context).Infof("not setting exited because process has scaled down: %v", p.Pid())
					continue
				}

				if w.scaledContainer.InitialProcess() != nil &&
					p.ID() == w.scaledContainer.InitialProcess().ID() ||
					p.ID() == w.scaledContainer.Process().ID() {
					// we also need to set the original process as being exited so we can exit cleanly
					w.scaledContainer.InitialProcess().SetExited(0)
				}
			}

			p.SetExited(e.Status)
			w.sendL(&eventstypes.TaskExit{
				ContainerID: container.ID,
				ID:          p.ID(),
				Pid:         uint32(e.Pid),
				ExitStatus:  uint32(e.Status),
				ExitedAt:    p.ExitedAt(),
			})
			return
		}
		return
	}
}

func (s *service) AddContainer(container *runc.Container) {
	s.containers[container.ID] = container
}
