package shim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/runc"
	"github.com/containerd/containerd/v2/pkg/stdio"
	"github.com/containerd/errdefs"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/activator"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
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
	initTimer        *time.Timer
	initBackoff      time.Duration
	platform         stdio.Platform
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
	runcVersion       string
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

	vers, err := (&runcC.Runc{}).Version(ctx)
	if err != nil {
		log.G(ctx).Warnf("unable to get runc version: %s", err)
	}
	log.G(ctx).Debugf("configuring zeropod shim with runc version %q", vers.Runc)

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
		runcVersion:       vers.Runc,
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

	if err := c.initActivator(ctx, c.SkipStart()); err != nil {
		log.G(ctx).Warnf("activator init failed, disabling scale down: %s", err)
		c.cfg.ScaleDownDuration = 0
	}
	if c.SkipStart() {
		c.setPhaseNotify(v1.ContainerPhase_SCALED_DOWN, 0)
		if err := c.scaleDown(ctx); err != nil {
			return err
		}
	} else {
		c.setPhaseNotify(v1.ContainerPhase_RUNNING, 0)
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
		if !c.activator.Started() {
			log.G(c.context).Infof("activator not ready, delaying scale down by %s", c.initBackoff)
			c.scaleDownTimer.Reset(c.initBackoff)
			return
		}
		last, err := c.lastActivity()
		if errors.Is(err, activator.NoActivityRecordedErr{}) {
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

func (c *Container) setPhase(phase v1.ContainerPhase) {
	switch phase {
	case v1.ContainerPhase_RUNNING:
		c.metrics.LastRestore = timestamppb.Now()
		c.scaledDown = false
		c.metrics.Running = true
	case v1.ContainerPhase_SCALED_DOWN:
		c.metrics.LastCheckpoint = timestamppb.Now()
		c.scaledDown = true
		c.metrics.Running = false
	}
}

func (c *Container) setPhaseNotify(phase v1.ContainerPhase, duration time.Duration) {
	c.setPhase(phase)
	switch phase {
	case v1.ContainerPhase_RUNNING:
		c.metrics.LastRestoreDuration = durationpb.New(duration)
	case v1.ContainerPhase_SCALED_DOWN:
		c.metrics.LastCheckpointDuration = durationpb.New(duration)
	}
	c.sendEvent(c.Status())
}

func (c *Container) setScaledDownFlag(flag bool) {
	c.scaledDown = flag
}

func (c *Container) SetSkipStart(skip bool) {
	c.skipStart = skip
}

func (c *Container) SkipStart() bool {
	return c.skipStart
}

func (c *Container) Status() *v1.ContainerStatus {
	phase := v1.ContainerPhase_RUNNING
	eventTime := c.metrics.LastRestore
	eventDuration := c.metrics.LastRestoreDuration
	if c.ScaledDown() {
		phase = v1.ContainerPhase_SCALED_DOWN
		eventTime = c.metrics.LastCheckpoint
		eventDuration = c.metrics.LastCheckpointDuration
	}
	return &v1.ContainerStatus{
		Id:            c.ID(),
		Name:          c.cfg.ContainerName,
		PodName:       c.cfg.PodName,
		PodNamespace:  c.cfg.PodNamespace,
		Phase:         phase,
		EventTime:     eventTime,
		EventDuration: eventDuration,
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
	c.cancelInit()
	c.CancelScaleDown()
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

func (c *Container) initActivator(ctx context.Context, enableRedirects bool) error {
	c.cancelInit()

	if c.activator == nil {
		act, err := activator.NewServer(ctx, c.netNS)
		if err != nil {
			return err
		}
		c.activator = act
	}

	if len(c.cfg.Ports) == 0 {
		log.G(ctx).Info("no ports defined in config, detecting listening ports")
		// if no ports are specified in the config, we try to find all listening ports
		ports, err := listeningPortsDeep(c.initialProcess.Pid())
		if err != nil || len(ports) == 0 {
			// our initialProcess might not even be running yet, so finding the listening
			// ports might fail in various ways. We schedule a retry.
			retryIn := c.initRetry()
			log.G(ctx).Infof("no ports detected, retrying init in %s", retryIn)
			c.retryInitIn(retryIn, enableRedirects)
			return nil
		}

		c.cfg.Ports = ports
	}

	log.G(ctx).Infof("starting activator with ports: %v", c.cfg.Ports)
	if err := c.startActivator(ctx, c.cfg.Ports...); err != nil {
		if errors.Is(err, activator.ErrMapNotFound) {
			c.retryInitIn(c.initRetry(), enableRedirects)
			return nil
		}
		return err
	}

	if enableRedirects {
		return c.activator.Reset()
	}
	return nil
}

// initRetry returns the duration in which the next init should be retried. It
// backs off exponentially with an initial wait of 100 milliseconds.
func (c *Container) initRetry() time.Duration {
	const initial, max = time.Millisecond * 100, time.Minute * 5
	c.initBackoff = min(max, c.initBackoff*2)

	if c.initBackoff == 0 {
		c.initBackoff = initial
	}

	return c.initBackoff
}

func (c *Container) retryInitIn(in time.Duration, enableRedirects bool) {
	log.G(c.context).Infof("scheduling init in %s", in)
	timer := time.AfterFunc(in, func() {
		if err := c.initActivator(c.context, enableRedirects); err != nil {
			log.G(c.context).Warnf("error initializing activator: %s", err)
		}
	})
	c.initTimer = timer
}

func (c *Container) cancelInit() {
	if c.initTimer == nil {
		return
	}
	c.initTimer.Stop()
}

// startActivator starts the activator
func (c *Container) startActivator(ctx context.Context, ports ...uint16) error {
	if c.activator.Started() {
		return nil
	}
	if err := c.activator.Start(c.context, c.detectProbe(c.context), c.restoreHandler(c.context), ports...); err != nil {
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

		restoredContainer, _, err := c.Restore(ctx)
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

		return c.ScheduleScaleDown()
	}
}

// lastActivity returns a [time.Time] of the last recorded network activity on
// any port of the container.
func (c *Container) lastActivity() (time.Time, error) {
	if c.activator == nil {
		return time.Time{}, activator.NoActivityRecordedErr{}
	}
	act := []time.Time{}
	for _, port := range c.cfg.Ports {
		last, err := c.activator.LastActivity(port)
		if err != nil {
			return time.Time{}, err
		}
		act = append(act, last)
	}
	if len(act) == 0 {
		return time.Time{}, activator.NoActivityRecordedErr{}
	}
	slices.SortFunc(act, func(a, b time.Time) int { return a.Compare(b) })
	return act[len(act)-1], nil
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
