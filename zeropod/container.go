package zeropod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/stdio"
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/activator"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/socket"
)

type HandleStartedFunc func(*runc.Container, process.Process)

type Container struct {
	*runc.Container

	context          context.Context
	activator        *activator.Server
	cfg              *Config
	initialProcess   process.Process
	process          process.Process
	cgroup           any
	logPath          string
	scaledDown       bool
	netNS            ns.NetNS
	scaleDownTimer   *time.Timer
	platform         stdio.Platform
	tracker          socket.Tracker
	preRestore       func() HandleStartedFunc
	postRestore      func(*runc.Container, HandleStartedFunc)
	events           chan *v1.ContainerStatus
	checkpointedPIDs map[int]struct{}
	pidsMu           sync.Mutex
	// mutex to lock during checkpoint/restore operations since concurrent
	// restores can cause cgroup confusion. This mutex is shared between all
	// containers.
	checkpointRestore *sync.Mutex
	evacuation        sync.Once
}

func New(ctx context.Context, cfg *Config, cr *sync.Mutex, container *runc.Container, pt stdio.Platform, events chan *v1.ContainerStatus) (*Container, error) {
	p, err := container.Process("")
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	// get network ns of our container and store it for later use
	netNSPath, err := GetNetworkNS(cfg.spec)
	if err != nil {
		return nil, err
	}

	targetNS, err := ns.GetNS(netNSPath)
	if err != nil {
		return nil, err
	}

	logPath, err := getLogPath(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to get log path: %w", err)
	}

	tracker, err := socket.NewEBPFTracker()
	if err != nil {
		log.G(ctx).Warnf("creating ebpf tracker failed, falling back to noop tracker: %s", err)
		tracker = socket.NewNoopTracker(cfg.ScaleDownDuration)
	}

	if err := tracker.TrackPid(uint32(p.Pid())); err != nil {
		return nil, err
	}

	c := &Container{
		Container:         container,
		context:           ctx,
		platform:          pt,
		cfg:               cfg,
		cgroup:            container.Cgroup(),
		process:           p,
		initialProcess:    p,
		logPath:           logPath,
		netNS:             targetNS,
		tracker:           tracker,
		checkpointRestore: cr,
		events:            events,
		checkpointedPIDs:  map[int]struct{}{},
	}

	running.With(c.labels()).Set(1)
	c.sendEvent(c.Status())

	return c, c.initActivator(ctx)
}

func (c *Container) ScheduleScaleDown() error {
	return c.scheduleScaleDownIn(c.cfg.ScaleDownDuration)
}

func (c *Container) scheduleScaleDownIn(in time.Duration) error {
	// cancel any potential pending scaledonws
	c.CancelScaleDown()

	log.G(c.context).Infof("scheduling scale down in %s", in)
	timer := time.AfterFunc(in, func() {
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

		if err := c.scaleDown(c.context); err != nil {
			// checkpointing failed, this is currently unrecoverable. We set our
			// initialProcess as exited to make sure it's restarted
			log.G(c.context).Errorf("scale down failed: %s", err)
			c.initialProcess.SetExited(1)
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
	c.sendEvent(c.Status())
}

func (c *Container) Status() *v1.ContainerStatus {
	phase := v1.ContainerPhase_RUNNING
	if c.ScaledDown() {
		phase = v1.ContainerPhase_SCALED_DOWN
	}
	return &v1.ContainerStatus{
		Id:           c.ID(),
		Name:         c.cfg.ContainerName,
		PodName:      c.cfg.PodName,
		PodNamespace: c.cfg.PodNamespace,
		Phase:        phase,
	}
}

func (c *Container) sendEvent(event *v1.ContainerStatus) {
	select {
	case c.events <- event:
	default:
		log.G(c.context).Infof("channel full, discarding event: %v", event)
	}
}

func (c *Container) ScaledDown() bool {
	return c.scaledDown
}

func (c *Container) ID() string {
	return c.Container.ID
}

func (c *Container) InitialProcess() process.Process {
	return c.initialProcess
}

func (c *Container) StopActivator(ctx context.Context) {
	c.activator.Stop(ctx)
}

// CheckpointedPID indicates if the pid has been checkpointed before.
func (c *Container) CheckpointedPID(pid int) bool {
	c.pidsMu.Lock()
	defer c.pidsMu.Unlock()
	_, ok := c.checkpointedPIDs[pid]
	return ok
}

// AddCheckpointedPID registers a new pid that should be considered checkpointed.
func (c *Container) AddCheckpointedPID(pid int) {
	c.pidsMu.Lock()
	defer c.pidsMu.Unlock()
	c.checkpointedPIDs[pid] = struct{}{}
}

// DeleteCheckpointedPID deletes a pid from the map of checkpointed pids.
func (c *Container) DeleteCheckpointedPID(pid int) {
	c.pidsMu.Lock()
	defer c.pidsMu.Unlock()
	delete(c.checkpointedPIDs, pid)
}

func (c *Container) Stop(ctx context.Context) {
	c.CancelScaleDown()
	if err := c.tracker.Close(); err != nil {
		log.G(ctx).Errorf("unable to close tracker: %s", err)
	}
	c.StopActivator(ctx)
	c.deleteMetrics()
}

func (c *Container) Process() process.Process {
	return c.process
}

func (c *Container) RegisterPreRestore(f func() HandleStartedFunc) {
	c.preRestore = f
}

func (c *Container) RegisterPostRestore(f func(*runc.Container, HandleStartedFunc)) {
	c.postRestore = f
}

var errNoPortsDetected = errors.New("no listening ports detected")

func (c *Container) initActivator(ctx context.Context) error {
	// we already have an activator
	if c.activator != nil {
		return nil
	}

	srv, err := activator.NewServer(ctx, c.netNS)
	if err != nil {
		return err
	}
	c.activator = srv

	return nil
}

// startActivator starts the activator
func (c *Container) startActivator(ctx context.Context) error {
	if c.activator.Started() {
		return nil
	}

	if len(c.cfg.Ports) == 0 {
		log.G(ctx).Info("no ports defined in config, detecting listening ports")
		// if no ports are specified in the config, we try to find all listening ports
		ports, err := listeningPortsDeep(c.initialProcess.Pid())
		if err != nil || len(ports) == 0 {
			// our initialProcess might not even be running yet, so finding the listening
			// ports might fail in various ways. We return errNoPortsDetected so the
			// caller can retry later.
			return errNoPortsDetected
		}

		c.cfg.Ports = ports
	}

	log.G(ctx).Infof("starting activator with ports: %v", c.cfg.Ports)

	// create a new context in order to not run into deadline of parent context
	ctx = log.WithLogger(context.Background(), log.G(ctx).WithField("runtime", RuntimeName))

	log.G(ctx).Infof("starting activator with config: %v", c.cfg)

	if err := c.activator.Start(ctx, c.cfg.Ports, c.restoreHandler(ctx)); err != nil {
		if errors.Is(err, activator.ErrMapNotFound) {
			return err
		}

		log.G(ctx).Errorf("failed to start activator: %s", err)
		return err
	}

	log.G(ctx).Printf("activator started")
	return nil
}

func (c *Container) restoreHandler(ctx context.Context) activator.OnAccept {
	return func() error {
		log.G(ctx).Printf("got a request")

		beforeRestore := time.Now()
		restoredContainer, p, err := c.Restore(ctx)
		if err != nil {
			if errors.Is(err, ErrAlreadyRestored) {
				log.G(ctx).Info("container is already restored, ignoring request")
				return nil
			}
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
		c.Container = restoredContainer

		if err := c.tracker.TrackPid(uint32(p.Pid())); err != nil {
			log.G(ctx).Errorf("unable to track pid %d: %s", p.Pid(), err)
		}

		log.G(ctx).Printf("restored process: %d in %s", p.Pid(), time.Since(beforeRestore))

		return c.ScheduleScaleDown()
	}
}

func snapshotDir(bundle string) string {
	return path.Join(bundle, "work", "snapshots")
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
