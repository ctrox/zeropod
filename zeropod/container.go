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
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/activator"
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
	process        process.Process
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
	netNSPath, err := GetNetworkNS(spec)
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
		process:        p,
		logPath:        logPath,
		netNS:          targetNS,
		activator:      srv,
		scheduler:      chrono.NewDefaultTaskScheduler(),
	}, nil
}

func (c *Container) ScheduleScaleDown(container *runc.Container) error {
	p := c.process

	if c.scaleDownTask != nil && !c.scaleDownTask.IsCancelled() {
		// cancel any potential pending scaledonws
		c.scaleDownTask.Cancel()
	}

	task, err := c.scheduler.Schedule(func(_ context.Context) {
		log.G(c.context).Info("scaling down after scale down duration is up")

		if err := c.scaleDown(c.context, container, p); err != nil {
			// checkpointing failed, this is currently unrecoverable, so we
			// shutdown our shim and let containerd recreate it.
			log.G(c.context).Fatalf("scale down failed: %s", err)
			os.Exit(1)
		}
	}, chrono.WithTime(time.Now().Add(c.cfg.ScaleDownDuration)))
	if err != nil {
		return err
	}

	c.scaleDownTask = task
	return nil
}

func (c *Container) CancelScaleDown() {
	c.scaleDownTask.Cancel()
}

func (c *Container) SetScaledDown(scaledDown bool) {
	c.scaledDown = scaledDown
}

func (c *Container) ScaledDown() bool {
	return c.scaledDown
}

func (c *Container) ID() string {
	return c.id
}

func (c *Container) InitialID() string {
	return c.initialID
}

func (c *Container) InitialProcess() process.Process {
	return c.initialProcess
}

func (c *Container) StopActivator(ctx context.Context) {
	c.activator.Stop(ctx)
}

func (c *Container) Process() process.Process {
	return c.process
}

// startActivator starts the activator
func (c *Container) startActivator(ctx context.Context, container *runc.Container) error {
	// create a new context in order to not run into deadline of parent context
	ctx = log.WithLogger(context.Background(), log.G(ctx).WithField("runtime", RuntimeName))

	log.G(ctx).Infof("starting activator with config: %v", c.cfg)

	if err := c.activator.Start(ctx, c.restoreHandler(ctx, container), c.checkpointHandler(ctx)); err != nil {
		log.G(ctx).Errorf("failed to start server: %s", err)
		return err
	}

	log.G(ctx).Printf("activator started")
	return nil
}

func (c *Container) restoreHandler(ctx context.Context, container *runc.Container) activator.AcceptFunc {
	return func() (*runc.Container, error) {
		log.G(ctx).Printf("got a request")

		// we "lock" the network to ensure no requests get a connection
		// refused while nothing is listening on the zeropod port. As soon as
		// the process is restored we remove the lock and let traffic flow.
		beforeLock := time.Now()
		if err := lockNetwork(c.netNS); err != nil {
			return nil, err
		}
		log.G(ctx).Printf("took %s to lock network", time.Since(beforeLock))

		beforeRestore := time.Now()
		restoredContainer, p, err := c.Restore(ctx, container)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
		c.scaledDown = false
		log.G(ctx).Printf("restored process: %d in %s", p.Pid(), time.Since(beforeRestore))

		beforeUnlock := time.Now()
		if err := unlockNetwork(c.netNS, 0); err != nil {
			return nil, err
		}
		log.G(ctx).Printf("took %s to unlock network", time.Since(beforeUnlock))

		return restoredContainer, nil
	}
}

func (c *Container) checkpointHandler(ctx context.Context) activator.ClosedFunc {
	return func(container *runc.Container) error {
		return c.ScheduleScaleDown(container)
	}
}

func snapshotDir(bundle string) string {
	return path.Join(bundle, "snapshots")
}

func containerDir(bundle string) string {
	return path.Join(snapshotDir(bundle), "container")
}

const preDumpDirName = "pre-dump"

func preDumpDir(bundle string) string {
	return path.Join(snapshotDir(bundle), preDumpDirName)
}

func relativePreDumpDir() string {
	return "../" + preDumpDirName
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
