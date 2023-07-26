package task

import (
	"context"
	"fmt"
	"os"
	"sync"
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
		service:           s,
		zeropodContainers: make(map[string]*zeropod.Container),
		checkpointRestore: sync.Mutex{},
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

	mut               sync.Mutex
	checkpointRestore sync.Mutex
	zeropodContainers map[string]*zeropod.Container
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

	w.mut.Lock()
	defer w.mut.Unlock()

	// if we have a sandbox container, an exec ID is set or the container does
	// not match the configured one(s) we should not do anything further with
	// the container.
	if cfg.ContainerType == annotations.ContainerTypeSandbox ||
		len(r.ExecID) != 0 ||
		!cfg.IsZeropodContainer() {
		log.G(ctx).Debugf("ignoring container: %q of type %q", cfg.ContainerName, cfg.ContainerType)
		return resp, nil
	}

	log.G(ctx).Infof("creating zeropod container: %s", cfg.ContainerName)

	zeropodContainer, err := zeropod.New(w.context, cfg, &w.checkpointRestore, container, w.platform)
	if err != nil {
		return nil, fmt.Errorf("error creating scaled container: %w", err)
	}

	zeropodContainer.RegisterSetContainer(func(c *runc.Container) {
		w.setContainer(c)
	})

	w.zeropodContainers[r.ID] = zeropodContainer

	w.shutdown.RegisterCallback(func(ctx context.Context) error {
		// stop server on shutdown
		zeropodContainer.Stop(ctx)
		return nil
	})

	// TODO: this is not a good idea (the 10s). A better idea is probably to
	// wait whenever we try to first get the Port from the app (retry until
	// the app is listening).
	if err := zeropodContainer.ScheduleScaleDown(); err != nil {
		return nil, err
	}

	return resp, err
}

func (w *wrapper) getZeropodContainer(id string) (*zeropod.Container, bool) {
	w.mut.Lock()
	container, ok := w.zeropodContainers[id]
	w.mut.Unlock()
	return container, ok
}

func (w *wrapper) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if !ok {
		return w.service.Exec(ctx, r)
	}

	zeropodContainer.CancelScaleDown()

	// restore it for exec in case we are scaled down
	if zeropodContainer.ScaledDown() {
		log.G(ctx).Printf("got exec for scaled down container, restoring")
		beforeRestore := time.Now()

		zeropodContainer.StopActivator(ctx)

		_, p, err := zeropodContainer.Restore(ctx)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}

		zeropodContainer.SetScaledDown(false)
		log.G(ctx).Printf("restored process for exec: %d in %s", p.Pid(), time.Since(beforeRestore))
	}

	return w.service.Exec(ctx, r)
}

func (w *wrapper) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if !ok {
		return w.service.Delete(ctx, r)
	}

	if len(r.ExecID) != 0 {
		// on delete of an exec container we want to schedule scaling down again.
		if err := zeropodContainer.ScheduleScaleDown(); err != nil {
			return nil, err
		}
	}
	return w.service.Delete(ctx, r)
}

func (w *wrapper) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	// our container might be just in the process of checkpoint/restore, so we
	// ensure that has finished.
	w.checkpointRestore.Lock()
	defer w.checkpointRestore.Unlock()

	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if !ok {
		return w.service.Kill(ctx, r)
	}

	if len(r.ExecID) == 0 && zeropodContainer.ScaledDown() {
		log.G(ctx).Infof("requested scaled down process %d to be killed", zeropodContainer.Process().Pid())
		zeropodContainer.Process().SetExited(0)
		zeropodContainer.InitialProcess().SetExited(0)

		return w.service.Kill(ctx, r)
	}

	if len(r.ExecID) == 0 {
		log.G(ctx).Infof("requested container %s to be killed", r.ID)
		zeropodContainer.Stop(ctx)

		if err := zeropodContainer.Process().Kill(ctx, r.Signal, r.All); err != nil {
			return nil, errdefs.ToGRPC(err)
		}

		zeropodContainer.InitialProcess().SetExited(0)
	}

	return w.service.Kill(ctx, r)
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

			zeropodContainer, ok := w.getZeropodContainer(container.ID)
			if ok {
				if zeropodContainer.ScaledDown() {
					log.G(w.context).Infof("not setting exited because process has scaled down: %v", p.Pid())
					continue
				}

				if zeropodContainer.InitialProcess() != nil &&
					p.ID() == zeropodContainer.InitialProcess().ID() ||
					p.ID() == zeropodContainer.Process().ID() {
					// we also need to set the original process as being exited so we can exit cleanly
					zeropodContainer.InitialProcess().SetExited(0)
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

// setContainer replaces the container in the task service. This is important
// to call after restore since the container object will have changed.
func (s *service) setContainer(container *runc.Container) {
	s.mu.Lock()
	s.containers[container.ID] = container
	s.mu.Unlock()
}
