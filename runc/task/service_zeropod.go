package task

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ctrox/zeropod/runc"
	"github.com/ctrox/zeropod/zeropod"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/sirupsen/logrus"

	"github.com/containerd/cgroups"
	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/cri/annotations"
	"github.com/containerd/containerd/pkg/oom"
	oomv1 "github.com/containerd/containerd/pkg/oom/v1"
	oomv2 "github.com/containerd/containerd/pkg/oom/v2"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/containerd/sys/reaper"
	runcC "github.com/containerd/go-runc"
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
	if err := s.initPlatform(); err != nil {
		return nil, fmt.Errorf("failed to initialized platform behavior: %w", err)
	}
	go s.forward(ctx, publisher)
	sd.RegisterCallback(func(context.Context) error {
		close(s.events)
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

func (s *wrapper) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	resp, err := s.service.Start(ctx, r)
	if err != nil {
		return nil, err
	}

	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	spec, err := runc.GetSpec(container.Bundle)
	if err != nil {
		return nil, err
	}

	cfg, err := zeropod.NewConfig(spec)
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

	scaledContainer, err := zeropod.New(s.context, spec, cfg, container, s.platform)
	if err != nil {
		return nil, err
	}

	s.shutdown.RegisterCallback(func(ctx context.Context) error {
		// stop server on shutdown
		scaledContainer.StopActivator(ctx)
		return nil
	})

	s.scaledContainer = scaledContainer

	if err := s.scaledContainer.ScheduleScaleDown(container); err != nil {
		return nil, err
	}

	return resp, err
}

func (s *wrapper) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}
	s.scaledContainer.CancelScaleDown()

	// restore it for exec in case we are scaled down
	if s.isScaledDownContainer(r.ID) {
		log.G(ctx).Printf("got exec for scaled down container, restoring")
		beforeRestore := time.Now()

		s.scaledContainer.StopActivator(ctx)

		p, err := s.scaledContainer.Restore(ctx, container)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
		s.scaledContainer.SetScaledDown(false)
		log.G(ctx).Printf("restored process for exec: %d in %s", p.Pid(), time.Since(beforeRestore))
	}

	return s.service.Exec(ctx, r)
}

func (s *wrapper) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	if len(r.ExecID) != 0 && r.ID == s.scaledContainer.InitialID() {
		container, err := s.getContainer(r.ID)
		if err != nil {
			return nil, err
		}

		// on delete of an exec container we want to schedule scaling down again.
		if err := s.scaledContainer.ScheduleScaleDown(container); err != nil {
			return nil, err
		}
	}
	return s.service.Delete(ctx, r)
}

func (s *wrapper) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	if len(r.ExecID) == 0 && s.isScaledDownContainer(r.ID) {
		container, err := s.getContainer(r.ID)
		if err != nil {
			return nil, err
		}
		p, err := container.Process("")
		if err != nil {
			return nil, err
		}
		log.G(ctx).Infof("requested scaled down process %d to be killed", p.Pid())
		s.scaledContainer.InitialProcess().SetExited(0)
		p.SetExited(0)
	}

	return s.service.Kill(ctx, r)
}

func (s *wrapper) isScaledDownContainer(id string) bool {
	return id == s.scaledContainer.InitialID() && s.scaledContainer.ScaledDown()
}

func (s *wrapper) processExits() {
	for e := range s.ec {
		s.checkProcesses(e)
	}
}

func (s *wrapper) checkProcesses(e runcC.Exit) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, container := range s.containers {
		if !container.HasPid(e.Pid) {
			continue
		}

		for _, p := range container.All() {
			if p.Pid() != e.Pid {
				continue
			}

			if ip, ok := p.(*process.Init); ok {
				// Ensure all children are killed
				if runc.ShouldKillAllOnExit(s.context, container.Bundle) {
					if err := ip.KillAll(s.context); err != nil {
						logrus.WithError(err).WithField("id", ip.ID()).
							Error("failed to kill init's children")
					}
				}
			}

			if s.scaledContainer.ScaledDown() && container.ID == s.scaledContainer.ID() {
				log.G(s.context).Infof("not setting exited because process has scaled down: %v", p.Pid())
				continue
			}

			main, err := container.Process("")
			if err != nil {
				continue
			}

			if s.scaledContainer.InitialProcess() != nil &&
				p.ID() == s.scaledContainer.InitialProcess().ID() ||
				p.ID() == main.ID() {
				// we also need to set the original process as being exited so we can exit cleanly
				s.scaledContainer.InitialProcess().SetExited(0)
			}

			p.SetExited(e.Status)
			s.sendL(&eventstypes.TaskExit{
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
