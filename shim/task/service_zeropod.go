package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/containerd/cgroups/v3"
	cgroupv2stats "github.com/containerd/cgroups/v3/cgroup2/stats"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/runc"
	"github.com/containerd/containerd/v2/pkg/oom"
	oomv1 "github.com/containerd/containerd/v2/pkg/oom/v1"
	oomv2 "github.com/containerd/containerd/v2/pkg/oom/v2"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/sys/reaper"
	"github.com/containerd/errdefs"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	"github.com/containerd/typeurl/v2"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	zshim "github.com/ctrox/zeropod/shim"
	"google.golang.org/protobuf/types/known/emptypb"
)

// containerTypeSandbox represents a pod sandbox container
// from github.com/containerd/containerd/v2/internal/cri/annotations
const containerTypeSandbox = "sandbox"

var (
	_ = (shim.TTRPCService)(&wrapper{})
)

func NewZeropodService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
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
		zeropodContainers: make(map[string]*zshim.Container),
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

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			if err := shim.RemoveSocket(shimSocketAddress(address)); err != nil {
				log.G(ctx).Errorf("removing zeropod socket: %s", err)
			}
			return shim.RemoveSocket(address)
		})
	}

	if id, err := shimID(); err == nil {
		go startShimServer(ctx, id, w)
	} else {
		log.G(ctx).Errorf("unable to get shim ID: %s", err)
	}

	return w, nil
}

type wrapper struct {
	*service

	mut               sync.Mutex
	checkpointRestore sync.Mutex
	zeropodContainers map[string]*zshim.Container
	zeropodEvents     chan *v1.ContainerStatus
	exitChan          chan runcC.Exit
}

func (w *wrapper) RegisterTTRPC(server *ttrpc.Server) error {
	// TODO: switch to taskServiceV3 once containerd 2 is widely adopted.
	registerTaskService(taskServiceV2, server, w)
	return nil
}

func (w *wrapper) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	spec, err := zshim.GetSpec(r.Bundle)
	if err != nil {
		return nil, err
	}

	cfg, err := zshim.NewConfig(ctx, spec)
	if err != nil {
		return nil, err
	}

	// if we have a sandbox container or the container does not match the
	// configured one(s) we should not do anything further with the container.
	if cfg.ContainerType == containerTypeSandbox ||
		!cfg.IsZeropodContainer() {
		log.G(ctx).Debugf("ignoring container: %q of type %q", cfg.ContainerName, cfg.ContainerType)
		return w.service.Create(ctx, r)
	}

	zeropodContainer, err := zshim.New(w.context, cfg, r, &w.checkpointRestore, w.platform, w.zeropodEvents)
	if err != nil {
		return nil, fmt.Errorf("error creating scaled container: %w", err)
	}

	w.setZeropodContainer(r.ID, zeropodContainer)
	zeropodContainer.RegisterPreRestore(func() zshim.HandleStartedFunc {
		return w.preRestore()
	})

	zeropodContainer.RegisterPostRestore(func(c *runc.Container, handleStarted zshim.HandleStartedFunc) {
		w.postRestore(c, handleStarted)
	})

	w.shutdown.RegisterCallback(func(ctx context.Context) error {
		// stop server on shutdown
		zeropodContainer.Stop(ctx)
		return nil
	})

	if cfg.AnyMigrationEnabled() {
		skipStart, err := zshim.MigrationRestore(ctx, r, cfg)
		if err != nil {
			if errors.Is(err, zshim.ErrRestoreRequestFailed) ||
				errors.Is(err, zshim.ErrRestoreDial) {
				// if the restore fails with ErrRestoreRequestFailed it's very
				// likely it simply did not find a matching migration. Equally,
				// if the shim can't manage to dial the node service there's no
				// chance it can be restored. We log it and create the container
				// from scratch.
				log.G(ctx).Errorf("restore request failed: %s", err)
			} else {
				return nil, err
			}
		}
		zeropodContainer.SetSkipStart(skipStart)
	}

	resp, err := w.service.Create(ctx, r)
	if err != nil {
		return nil, err
	}

	if cfg.AnyMigrationEnabled() && !cfg.DisableMigrateData {
		if err := zshim.MoveImageToUpperDir(r.ID, r.Checkpoint); err != nil {
			log.G(ctx).Errorf("restoring container data: %s", err)
		}
	}

	return resp, nil
}

func (w *wrapper) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).Infof("start called in zeropod service %s, %s", r.ID, r.ExecID)

	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if !ok || r.ExecID != "" {
		return w.service.Start(ctx, r)
	}

	container, err := w.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	beforeStart := time.Now()
	var resp *taskAPI.StartResponse
	if !zeropodContainer.SkipStart() {
		log.G(ctx).Infof("starting zeropod container: %s", zeropodContainer.Config().ContainerName)
		resp, err = w.service.Start(ctx, r)
		if err != nil {
			return nil, err
		}
	} else {
		log.G(ctx).Infof("skipping start of migrated zeropod container: %s", zeropodContainer.Config().ContainerName)
		resp = &taskAPI.StartResponse{
			Pid: 0,
		}
	}

	if err := zeropodContainer.Register(ctx, container); err != nil {
		return nil, fmt.Errorf("registering container: %w", err)
	}

	if zeropodContainer.Config().AnyMigrationEnabled() {
		if err := zshim.FinishRestore(ctx, r.ID, zeropodContainer.Config(), beforeStart); err != nil {
			log.G(ctx).Errorf("error finishing restore: %s", err)
		}
	}
	if zeropodContainer.SkipStart() {
		return resp, nil
	}

	if err := zeropodContainer.ScheduleScaleDown(); err != nil {
		return nil, err
	}

	return resp, err
}

func (w *wrapper) setZeropodContainer(id string, container *zshim.Container) {
	w.mut.Lock()
	w.zeropodContainers[id] = container
	w.mut.Unlock()
}

func (w *wrapper) getZeropodContainer(id string) (*zshim.Container, bool) {
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

		_, _, err := zeropodContainer.Restore(ctx)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
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

// Update a running container
func (w *wrapper) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*emptypb.Empty, error) {
	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if !ok || !zeropodContainer.ScaledDown() {
		return w.service.Update(ctx, r)
	}

	log.G(ctx).Infof("ignoring update for scaled down zeropod: %s", zeropodContainer.ID())
	return &emptypb.Empty{}, nil
}

func (w *wrapper) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	zeropodContainer, ok := w.getZeropodContainer(r.ID)
	if ok && zeropodContainer.ScaledDown() {
		return scaledDownStats()
	}
	return w.service.Stats(ctx, r)
}

func scaledDownStats() (*taskAPI.StatsResponse, error) {
	// if everything is zero, the metrics server discards the metrics so we set
	// 1 for cpu/memory usage.
	metrics := &cgroupv2stats.Metrics{
		CPU: &cgroupv2stats.CPUStat{
			UsageUsec: 1,
		},
		Memory: &cgroupv2stats.MemoryStat{
			Usage: 1,
		},
	}

	data, err := typeurl.MarshalAny(metrics)
	if err != nil {
		return nil, err
	}
	return &taskAPI.StatsResponse{
		Stats: typeurl.MarshalProto(data),
	}, nil
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
	zeropodContainer.CancelScaleDown()

	if len(r.ExecID) == 0 {
		if zeropodContainer.MigrationEnabled() {
			log.G(ctx).Info("migrating instead of killing process")
			if err := zeropodContainer.Evac(ctx, zeropodContainer.ScaledDown()); err != nil {
				log.G(ctx).WithError(err).Error("evac failed, exiting normally")
			}
		}

		if zeropodContainer.ScaledDown() {
			log.G(ctx).Infof("requested scaled down process %d to be killed", zeropodContainer.Process().Pid())
			zeropodContainer.Process().SetExited(0)
			zeropodContainer.InitialProcess().SetExited(0)
			zeropodContainer.Stop(ctx)

			return w.service.Kill(ctx, r)
		}

		log.G(ctx).Infof("requested container %s to be killed", r.ID)
		zeropodContainer.Stop(ctx)

		if err := zeropodContainer.Process().Kill(ctx, r.Signal, r.All); err != nil {
			return nil, errdefs.Resolve(err)
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
func (w *wrapper) preRestore() zshim.HandleStartedFunc {
	handleStarted, cleanup := w.preStart(nil)
	defer cleanup()
	return handleStarted
}

// postRestore replaces the container in the task service. This is important
// to call after restore since the container object will have changed.
// Additionally, this also calls the passed in handleStarted to make sure we
// monitor the process exits of the newly restored process.
func (w *wrapper) postRestore(container *runc.Container, handleStarted zshim.HandleStartedFunc) {
	w.mu.Lock()
	p, _ := container.Process("")
	w.containers[container.ID] = container
	w.mu.Unlock()

	if handleStarted != nil {
		handleStarted(container, p)
	}
}
