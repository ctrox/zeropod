package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/containerd/cgroups/v3"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/pkg/cri/annotations"
	"github.com/containerd/containerd/pkg/oom"
	oomv1 "github.com/containerd/containerd/pkg/oom/v1"
	oomv2 "github.com/containerd/containerd/pkg/oom/v2"
	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/containerd/sys/reaper"
	"github.com/containerd/errdefs"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/zeropod"
	"google.golang.org/protobuf/types/known/emptypb"
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
		context:              ctx,
		events:               make(chan interface{}, 128),
		ec:                   make(chan runcC.Exit, 32),
		ep:                   ep,
		shutdown:             sd,
		containers:           make(map[string]*runc.Container),
		running:              make(map[int][]containerProcess),
		runningExecs:         make(map[*runc.Container]int),
		execCountSubscribers: make(map[*runc.Container]chan<- int),
		containerInitExit:    make(map[*runc.Container]runcC.Exit),
		exitSubscribers:      make(map[*map[int][]runcC.Exit]struct{}),
	}
	w := &wrapper{
		service:           s,
		checkpointRestore: sync.Mutex{},
		zeropodContainers: make(map[string]*zeropod.Container),
		zeropodEvents:     make(chan *v1.ContainerStatus, 128),
		exitChan:          reaper.Default.Subscribe(),
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

	address, err := shim.ReadAddress("address")
	if err != nil {
		return nil, err
	}
	sd.RegisterCallback(func(context.Context) error {
		if err := shim.RemoveSocket(shimSocketAddress(address)); err != nil {
			log.G(ctx).Errorf("removing zeropod socket: %s", err)
		}
		return shim.RemoveSocket(address)
	})

	go startShimServer(ctx, address, w.zeropodEvents)

	return w, nil
}

type wrapper struct {
	*service

	mut               sync.Mutex
	checkpointRestore sync.Mutex
	migrate           sync.Mutex
	zeropodContainers map[string]*zeropod.Container
	zeropodEvents     chan *v1.ContainerStatus
	exitChan          chan runcC.Exit
}

func (w *wrapper) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTaskService(server, w)
	return nil
}

func (w *wrapper) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	spec, err := zeropod.GetSpec(r.Bundle)
	if err != nil {
		return nil, err
	}

	cfg, err := zeropod.NewConfig(ctx, spec)
	if err != nil {
		return nil, err
	}

	if cfg.MigrationEnabled() {
		if err := zeropod.CreateLazyRestore(ctx, r, cfg); err != nil {
			if !errors.Is(err, zeropod.ErrRestoreRequestFailed) {
				return nil, err
			}
			// if the restore fails with ErrRestoreRequestFailed it's very
			// likely it simply did not find a matching migration. We log it and
			// create the container from scratch.
			log.G(ctx).Errorf("restore request failed: %s", err)
		}
	}

	return w.service.Create(ctx, r)
}

func (w *wrapper) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).Infof("start called in zeropod service %s, %s", r.ID, r.ExecID)

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

	startCtx := ctx
	if cfg.MigrationEnabled() {
		restoreCtx, cancel := context.WithTimeout(ctx, time.Second*15)
		defer cancel()
		startCtx = restoreCtx
	}

	beforeStart := time.Now()
	resp, err := w.service.Start(startCtx, r)
	if err != nil {
		return nil, err
	}

	if cfg.MigrationEnabled() {
		if err := zeropod.FinishRestore(ctx, cfg, beforeStart); err != nil {
			log.G(ctx).Errorf("error finishing restore: %s", err)
		}
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

	zeropodContainer, err := zeropod.New(w.context, cfg, &w.checkpointRestore, container, w.platform, w.zeropodEvents)
	if err != nil {
		return nil, fmt.Errorf("error creating scaled container: %w", err)
	}

	zeropodContainer.RegisterPreRestore(func() zeropod.HandleStartedFunc {
		return w.preRestore()
	})

	zeropodContainer.RegisterPostRestore(func(c *runc.Container, handleStarted zeropod.HandleStartedFunc) {
		w.postRestore(c, handleStarted)
	})

	w.zeropodContainers[r.ID] = zeropodContainer

	w.shutdown.RegisterCallback(func(ctx context.Context) error {
		// stop server on shutdown
		zeropodContainer.Stop(ctx)
		return nil
	})

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

func (w *wrapper) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if !ok {
		return w.service.Exec(ctx, r)
	}

	zeropodContainer.CancelScaleDown()

	// restore it for exec in case we are scaled down
	if zeropodContainer.ScaledDown() {
		log.G(ctx).Printf("got exec for scaled down container, restoring")
		beforeRestore := time.Now()

		_, p, err := zeropodContainer.Restore(ctx)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}

		log.G(ctx).Printf("restored process for exec: %d in %s", p.Pid(), time.Since(beforeRestore))
	}

	return w.service.Exec(ctx, r)
}

func (w *wrapper) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if ok && zeropodContainer.ScaledDown() {
		log.G(ctx).Debugf("pids of scaled down container, returning last known pid: %v", zeropodContainer.Pid())
		return &taskAPI.PidsResponse{
			Processes: []*task.ProcessInfo{{Pid: uint32(zeropodContainer.Pid())}},
		}, nil
	}

	return w.service.Pids(ctx, r)
}

func (w *wrapper) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	log.G(ctx).Info("delete called")
	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if !ok {
		return w.service.Delete(ctx, r)
	}
	log.G(ctx).Infof("delete called in zeropod: %s", zeropodContainer.ID())

	if len(r.ExecID) != 0 {
		// on delete of an exec container we want to schedule scaling down again.
		if err := zeropodContainer.ScheduleScaleDown(); err != nil {
			return nil, err
		}
	}
	return w.service.Delete(ctx, r)
}

func (w *wrapper) Kill(ctx context.Context, r *taskAPI.KillRequest) (*emptypb.Empty, error) {
	// our container might be just in the process of checkpoint/restore, so we
	// ensure that has finished.
	w.checkpointRestore.Lock()
	defer w.checkpointRestore.Unlock()

	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if !ok {
		return w.service.Kill(ctx, r)
	}
	log.G(ctx).Infof("kill called in zeropod: %s", zeropodContainer.ID())

	if len(r.ExecID) == 0 && zeropodContainer.ScaledDown() {
		log.G(ctx).Infof("requested scaled down process %d to be killed", zeropodContainer.Process().Pid())
		zeropodContainer.Process().SetExited(0)
		zeropodContainer.InitialProcess().SetExited(0)
		zeropodContainer.Stop(ctx)

		return w.service.Kill(ctx, r)
	}

	if len(r.ExecID) == 0 {
		if zeropodContainer.MigrationEnabled() {
			log.G(ctx).Info("migrating instead of killing process")
			if err := zeropodContainer.Evac(ctx); err != nil {
				log.G(ctx).WithError(err).Error("evac failed, exiting normally")
				return w.service.Kill(ctx, r)
			}
			return empty, nil
		}

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
	go w.service.processExits()
	for e := range w.exitChan {
		w.lifecycleMu.Lock()
		cps := w.running[e.Pid]
		w.lifecycleMu.Unlock()
		preventExit := false
		for _, cp := range cps {
			if w.preventExit(cp) {
				preventExit = true
			}
		}
		if !preventExit {
			// pass event to service exit channel
			w.service.ec <- e
		}
	}
}

func (w *wrapper) preventExit(cp containerProcess) bool {
	zeropodContainer, ok := w.getZeropodContainer(cp.Container.ID)
	if ok {
		if zeropodContainer.ScaledDown() {
			log.G(w.context).Infof("not setting exited because process has scaled down: %v", cp.Process.Pid())
			return true
		}

		if zeropodContainer.CheckpointedPID(cp.Process.Pid()) {
			log.G(w.context).Infof("not setting exited because process has been checkpointed: %v", cp.Process.Pid())
			zeropodContainer.DeleteCheckpointedPID(cp.Process.Pid())
			return true
		}

		// we need to set the original process as being exited so we can exit cleanly
		if zeropodContainer.InitialProcess() != nil &&
			cp.Process.ID() == zeropodContainer.InitialProcess().ID() ||
			cp.Process.ID() == zeropodContainer.Process().ID() {
			zeropodContainer.InitialProcess().SetExited(0)
		}
	}

	return false
}

// preRestore should be called before restoring as it calls preStart in the
// task service to get the handleStarted closure.
func (w *wrapper) preRestore() zeropod.HandleStartedFunc {
	handleStarted, cleanup := w.preStart(nil)
	defer cleanup()
	return handleStarted
}

// postRestore replaces the container in the task service. This is important
// to call after restore since the container object will have changed.
// Additionally, this also calls the passed in handleStarted to make sure we
// monitor the process exits of the newly restored process.
func (w *wrapper) postRestore(container *runc.Container, handleStarted zeropod.HandleStartedFunc) {
	w.mu.Lock()
	p, _ := container.Process("")
	w.containers[container.ID] = container
	w.mu.Unlock()

	if handleStarted != nil {
		handleStarted(container, p)
	}
}
