package task

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/zeropod"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/containerd/cgroups"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/pkg/cri/annotations"
	"github.com/containerd/containerd/pkg/oom"
	oomv1 "github.com/containerd/containerd/pkg/oom/v1"
	oomv2 "github.com/containerd/containerd/pkg/oom/v2"
	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/containerd/sys/reaper"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
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
		context:         ctx,
		events:          make(chan interface{}, 128),
		ec:              reaper.Default.Subscribe(),
		ep:              ep,
		shutdown:        sd,
		containers:      make(map[string]*runc.Container),
		running:         make(map[int][]containerProcess),
		exitSubscribers: make(map[*map[int][]runcC.Exit]struct{}),
	}
	w := &wrapper{
		service:           s,
		checkpointRestore: sync.Mutex{},
		zeropodContainers: make(map[string]*zeropod.Container),
		zeropodEvents:     make(chan *v1.ContainerStatus, 128),
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
	zeropodContainers map[string]*zeropod.Container
	zeropodEvents     chan *v1.ContainerStatus
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

func (w *wrapper) Kill(ctx context.Context, r *taskAPI.KillRequest) (*emptypb.Empty, error) {
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
		zeropodContainer.Stop(ctx)

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
		cps := w.running[e.Pid]
		preventExit := false
		for _, cp := range cps {
			if w.preventExit(cp) {
				preventExit = true
			}
		}
		if preventExit {
			continue
		}

		// While unlikely, it is not impossible for a container process to exit
		// and have its PID be recycled for a new container process before we
		// have a chance to process the first exit. As we have no way to tell
		// for sure which of the processes the exit event corresponds to (until
		// pidfd support is implemented) there is no way for us to handle the
		// exit correctly in that case.

		w.lifecycleMu.Lock()
		// Inform any concurrent s.Start() calls so they can handle the exit
		// if the PID belongs to them.
		for subscriber := range w.exitSubscribers {
			(*subscriber)[e.Pid] = append((*subscriber)[e.Pid], e)
		}
		// Handle the exit for a created/started process. If there's more than
		// one, assume they've all exited. One of them will be the correct
		// process.
		delete(w.running, e.Pid)
		w.lifecycleMu.Unlock()

		for _, cp := range cps {
			w.mu.Lock()
			w.handleProcessExit(e, cp.Container, cp.Process)
			w.mu.Unlock()
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
		handleStarted(container, p, false)
	}
}
