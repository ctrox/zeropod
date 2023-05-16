package zeropod

import (
	"context"
	"os"
	"path"
	"strings"
	"time"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/integration/remote"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/stdio"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/activator"
	"github.com/ctrox/zeropod/runc"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/procyon-projects/chrono"
)

type Container struct {
	context        context.Context
	activator      *activator.Server
	spec           *specs.Spec
	cfg            *Config
	id             string
	initialID      string
	initialProcess process.Process
	logPath        string
	scaledDown     bool
	netNS          ns.NetNS
	scheduler      chrono.TaskScheduler
	scaleDownTask  chrono.ScheduledTask
	platform       stdio.Platform
}

func New(ctx context.Context, spec *specs.Spec, cfg *Config, container *runc.Container, pt stdio.Platform) (*Container, error) {
	p, err := container.Process("")
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	// get network ns of our container and store it for later use
	netNSPath, err := runc.GetNetworkNS(spec)
	if err != nil {
		return nil, err
	}

	targetNS, err := ns.GetNS(netNSPath)
	if err != nil {
		return nil, err
	}

	srv, err := activator.NewServer(ctx, cfg.Port, targetNS)
	if err != nil {
		return nil, err
	}

	logPath, err := getLogPath(ctx, container.ID)
	if err != nil {
		return nil, err
	}

	return &Container{
		context:        ctx,
		platform:       pt,
		cfg:            cfg,
		spec:           spec,
		id:             container.ID,
		initialID:      container.ID,
		initialProcess: p,
		logPath:        logPath,
		netNS:          targetNS,
		activator:      srv,
		scheduler:      chrono.NewDefaultTaskScheduler(),
	}, nil
}

func (s *Container) ScheduleScaleDown(container *runc.Container) error {
	p, err := container.Process("")
	if err != nil {
		return err
	}

	if s.scaleDownTask != nil && !s.scaleDownTask.IsCancelled() {
		// cancel any potential pending scaledonws
		s.scaleDownTask.Cancel()
	}

	task, err := s.scheduler.Schedule(func(_ context.Context) {
		log.G(s.context).Info("scaling down after scale down duration is up")

		if err := s.scaleDown(s.context, container, p); err != nil {
			// checkpointing failed, this is currently unrecoverable, so we
			// shutdown our shim and let containerd recreate it.
			log.G(s.context).Fatalf("scale down failed: %s", err)
			os.Exit(1)
		}
	}, chrono.WithTime(time.Now().Add(s.cfg.ScaleDownDuration)))
	if err != nil {
		return err
	}

	s.scaleDownTask = task
	return nil
}

func (s *Container) CancelScaleDown() {
	s.scaleDownTask.Cancel()
}

func (s *Container) SetScaledDown(scaledDown bool) {
	s.scaledDown = scaledDown
}

func (s *Container) ScaledDown() bool {
	return s.scaledDown
}

func (s *Container) ID() string {
	return s.id
}

func (s *Container) InitialID() string {
	return s.initialID
}

func (s *Container) InitialProcess() process.Process {
	return s.initialProcess
}

func (s *Container) StopActivator(ctx context.Context) {
	s.activator.Stop(ctx)
}

// startActivator starts the activator
func (s *Container) startActivator(ctx context.Context, container *runc.Container) error {
	// create a new context in order to not run into deadline of parent context
	ctx = log.WithLogger(context.Background(), log.G(ctx).WithField("runtime", runc.RuntimeName))

	log.G(ctx).Infof("starting activator with config: %v", s.cfg)

	if err := s.activator.Start(ctx, s.restoreHandler(ctx, container), s.checkpointHandler(ctx)); err != nil {
		log.G(ctx).Errorf("failed to start server: %s", err)
		return err
	}

	log.G(ctx).Printf("activator started")
	return nil
}

func (s *Container) restoreHandler(ctx context.Context, container *runc.Container) activator.AcceptFunc {
	return func() (*runc.Container, process.Process, error) {
		log.G(ctx).Printf("got a request")
		beforeRestore := time.Now()

		p, err := s.Restore(ctx, container)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
		s.scaledDown = false
		log.G(ctx).Printf("restored process: %d in %s", p.Pid(), time.Since(beforeRestore))

		// before returning we set the net ns again as it might have changed
		// in the meantime. (not sure why that happens though)
		return container, p, nil
	}
}

func (s *Container) checkpointHandler(ctx context.Context) activator.ClosedFunc {
	return func(container *runc.Container, p process.Process) error {
		return s.ScheduleScaleDown(container)
	}
}

func snapshotDir(bundle string) string {
	return path.Join(bundle, "snapshots")
}

func containerDir(bundle string) string {
	return path.Join(snapshotDir(bundle), "container")
}

// getLogPath gets the log path of the container by connecting back to
// containerd. There might be a less convoluted way to do this.
func getLogPath(ctx context.Context, containerID string) (string, error) {
	endpoint := os.Getenv("GRPC_ADDRESS")
	if len(endpoint) == 0 {
		endpoint = strings.TrimSuffix(os.Getenv("TTRPC_ADDRESS"), ".ttrpc")
	}

	cri, err := remote.NewRuntimeService(endpoint, time.Second)
	if err != nil {
		return "", err
	}

	status, err := cri.ContainerStatus(containerID)
	if err != nil {
		return "", err
	}

	return status.LogPath, nil
}
