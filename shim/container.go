package shim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/runc"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/activator"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/socket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type HandleStartedFunc func(*runc.Container, process.Process)

type Container struct {
	*runc.Container

	context          context.Context
	id               string
	createOpts       *anypb.Any
	activator        *activator.Server
	cfg              *Config
	initialProcess   process.Process
	process          process.Process
	cgroup           any
	logPath          string
	scaledDown       bool
	skipStart        bool
	netNS            ns.NetNS
	scaleDownTimer   *time.Timer
	scaleDownBackoff time.Duration
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
	metrics           *v1.ContainerMetrics
}

func New(ctx context.Context, cfg *Config, r *taskAPI.CreateTaskRequest, cr *sync.Mutex, pt stdio.Platform, events chan *v1.ContainerStatus) (*Container, error) {
	// get network ns of our container and store it for later use
	netNSPath, err := GetNetworkNS(cfg.spec)
	if err != nil {
		return nil, err
	}

	targetNS, err := ns.GetNS(netNSPath)
	if err != nil {
		return nil, err
	}

	logPath, err := getLogPath(cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to get log path: %w", err)
	}

	c := &Container{
		id:                r.ID,
		createOpts:        r.Options,
		context:           ctx,
		platform:          pt,
		cfg:               cfg,
		logPath:           logPath,
		netNS:             targetNS,
		checkpointRestore: cr,
		events:            events,
		checkpointedPIDs:  map[int]struct{}{},
		metrics:           newMetrics(cfg, true),
	}
	return c, nil
}

func (c *Container) Register(ctx context.Context, container *runc.Container) error {
	c.Container = container
	c.cgroup = container.Cgroup()

	p, err := container.Process("")
	if err != nil {
		return errdefs.Resolve(err)
	}
	c.process = p
	c.initialProcess = p

	tracker, err := socket.NewEBPFTracker()
	if err != nil {
		log.G(ctx).Warnf("creating ebpf tracker failed, falling back to noop tracker: %s", err)
		tracker = socket.NewNoopTracker(c.cfg.ScaleDownDuration)
	}
	c.tracker = tracker

	if err := tracker.TrackPid(uint32(p.Pid())); err != nil {
		log.G(ctx).Warnf("tracking pid failed: %s", err)
	}
	if err := c.initActivator(ctx); err != nil {
		log.G(ctx).Warnf("activator init failed, disabling scale down: %s", err)
		c.cfg.ScaleDownDuration = 0
	}
	if c.SkipStart() {
		c.SetScaledDown(true)
		if err := c.scaleDown(ctx); err != nil {
			return err
		}
	} else {
		c.SetScaledDown(false)
	}
	return nil
}

func (c *Container) Config() *Config {
	return c.cfg
}

func (c *Container) ScheduleScaleDown() error {
	return c.scheduleScaleDownIn(c.cfg.ScaleDownDuration)
}

func (c *Container) scheduleScaleDownIn(in time.Duration) error {
	// cancel any potential pending scaledonws
	c.CancelScaleDown()

	if in == 0 {
		log.G(c.context).Info("scale down is disabled")
		return nil
	}

	log.G(c.context).Infof("scheduling scale down in %s", in)
	timer := time.AfterFunc(in, func() {
		last, err := c.tracker.LastActivity(uint32(c.process.Pid()))
		if errors.Is(err, socket.NoActivityRecordedErr{}) {
			log.G(c.context).Info(err)
		} else if err != nil {
			log.G(c.context).Warnf("unable to get last TCP activity from tracker: %s", err)
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
	c.metrics.Running = !scaledDown
	if scaledDown {
		c.metrics.LastCheckpoint = timestamppb.Now()
	} else {
		c.metrics.LastRestore = timestamppb.Now()
	}
	c.sendEvent(c.Status())
}

func (c *Container) SetSkipStart(skip bool) {
	c.skipStart = skip
}

func (c *Container) SkipStart() bool {
	return c.skipStart
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
	return c.id
}

func (c *Container) InitialProcess() process.Process {
	return c.initialProcess
}

func (c *Container) StopActivator(ctx context.Context) {
	if c.activator != nil {
		c.activator.Stop(ctx)
	}
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
	if err := c.tracker.RemovePid(uint32(c.process.Pid())); err != nil {
		log.G(ctx).Warnf("unable to remove pid from tracker: %s", err)
	}
	if err := c.tracker.Close(); err != nil {
		log.G(ctx).Warnf("unable to close tracker: %s", err)
	}
	status := c.Status()
	status.Phase = v1.ContainerPhase_STOPPING
	c.sendEvent(status)
	c.StopActivator(ctx)
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

	if err := c.activator.Start(ctx, c.cfg.Ports, c.detectProbe(ctx), c.restoreHandler(ctx)); err != nil {
		if errors.Is(err, activator.ErrMapNotFound) {
			return err
		}

		log.G(ctx).Errorf("failed to start activator: %s", err)
		return err
	}

	log.G(ctx).Printf("activator started")
	return nil
}

func (c *Container) restoreHandler(ctx context.Context) activator.RestoreHook {
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
			log.G(ctx).Warnf("unable to track pid %d: %s", p.Pid(), err)
		}

		log.G(ctx).Printf("restored process: %d in %s", p.Pid(), time.Since(beforeRestore))

		return c.ScheduleScaleDown()
	}
}

func (c *Container) GetMetrics() *v1.ContainerMetrics {
	m := proto.Clone(c.metrics)
	c.clearMetrics()
	return m.(*v1.ContainerMetrics)
}

func (c *Container) clearMetrics() {
	c.metrics.LastCheckpoint = nil
	c.metrics.LastRestore = nil
	c.metrics.LastCheckpointDuration = nil
	c.metrics.LastRestoreDuration = nil
}
