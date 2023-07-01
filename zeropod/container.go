package zeropod

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/ctrox/zeropod/socket"
	"github.com/opencontainers/runtime-spec/specs-go"
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
	scaleDownTimer *time.Timer
	platform       stdio.Platform
	tracker        socket.Tracker
	stopMetrics    context.CancelFunc
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

	tracker, err := socket.NewEBPFTracker()
	if err != nil {
		log.G(ctx).Warnf("creating ebpf tracker failed, falling back to noop tracker: %s", err)
		tracker = socket.NewNoopTracker(cfg.ScaleDownDuration)
	}

	if err := tracker.TrackPid(uint32(p.Pid())); err != nil {
		return nil, err
	}

	metricsCtx, stopMetrics := context.WithCancel(ctx)
	go startMetricsServer(metricsCtx, container.ID)

	c := &Container{
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
		tracker:        tracker,
		stopMetrics:    stopMetrics,
	}

	running.With(c.labels()).Set(1)

	return c, nil
}

func (c *Container) ScheduleScaleDown(container *runc.Container) error {
	return c.scheduleScaleDown(container, c.cfg.ScaleDownDuration)
}

func (c *Container) scheduleScaleDown(container *runc.Container, in time.Duration) error {
	// cancel any potential pending scaledonws
	c.CancelScaleDown()

	log.G(c.context).Infof("scheduling scale down in %s", c.cfg.ScaleDownDuration)
	timer := time.AfterFunc(c.cfg.ScaleDownDuration, func() {
		last, err := c.tracker.LastActivity(uint32(c.process.Pid()))
		if errors.Is(err, socket.NoActivityRecordedErr{}) {
			log.G(c.context).Info(err)
		} else if err != nil {
			log.G(c.context).Errorf("unable to get last TCP activity from tracker: %s", err)
		} else {
			log.G(c.context).Infof("last activity was %s ago", time.Since(last))

			if time.Since(last) < c.cfg.ScaleDownDuration {
				// we want to delay the scaledown by c.cfg.ScaleDownDuration
				// after the last activity
				delay := c.cfg.ScaleDownDuration - time.Since(last)
				// do not schedule into the past :)
				if delay < 0 {
					return
				}

				log.G(c.context).Infof("delaying scale down by %s", delay)
				c.scaleDownTimer.Reset(delay)
				return
			}
		}

		log.G(c.context).Info("scaling down after scale down duration is up")

		if err := c.scaleDown(c.context, container, c.process); err != nil {
			// checkpointing failed, this is currently unrecoverable, so we
			// shutdown our shim and let containerd recreate it.
			log.G(c.context).Fatalf("scale down failed: %s", err)
			os.Exit(1)
		}

	})
	c.scaleDownTimer = timer
	return nil
}

func (c *Container) CancelScaleDown() {
	if c.scaleDownTimer == nil {
		return
	}

	c.scaleDownTimer.Stop()
}

func (c *Container) SetScaledDown(scaledDown bool) {
	c.scaledDown = scaledDown
	if scaledDown {
		running.With(c.labels()).Set(0)
		lastCheckpointTime.With(c.labels()).Set(float64(time.Now().UnixNano()))
	} else {
		running.With(c.labels()).Set(1)
		lastRestoreTime.With(c.labels()).Set(float64(time.Now().UnixNano()))
	}
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

func (c *Container) Stop(ctx context.Context) {
	c.CancelScaleDown()
	if err := c.tracker.Close(); err != nil {
		log.G(ctx).Errorf("unable to close tracker: %s", err)
	}
	c.StopActivator(ctx)
	c.stopMetrics()
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

func (c *Container) restoreHandler(ctx context.Context, container *runc.Container) activator.OnAccept {
	return func() (*runc.Container, error) {
		log.G(ctx).Printf("got a request")

		// we "lock" the network to ensure no requests get a connection
		// refused while nothing is listening on the zeropod port. As soon as
		// the process is restored we remove the lock and let traffic flow.

		beforeRestore := time.Now()
		restoredContainer, p, err := c.Restore(ctx, container)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}

		if err := c.tracker.TrackPid(uint32(p.Pid())); err != nil {
			return nil, fmt.Errorf("unable to track pid %d: %w", p.Pid(), err)
		}

		c.SetScaledDown(false)
		restoreDuration.With(c.labels()).Observe(time.Since(beforeRestore).Seconds())

		log.G(ctx).Printf("restored process: %d in %s", p.Pid(), time.Since(beforeRestore))

		return restoredContainer, nil
	}
}

func (c *Container) checkpointHandler(ctx context.Context) activator.OnClosed {
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
