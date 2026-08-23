// Package shim contains the zeropod container handling
package shim

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/ctrox/zeropod/activator/reuse"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type HandleStartedFunc func(*runc.Container, process.Process)

type Container struct {
	*runc.Container
	// mutex to lock during checkpoint/restore operations to ensure we don't try
	// to restore during checkpoint or the other way around.
	CheckpointRestore *sync.Mutex

	context          context.Context
	id               string
	createOpts       *anypb.Any
	activator        activator.Activator
	cfg              *v1.Config
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
	evacDrainStarted atomic.Bool
	drainTimer       *time.Timer
	drainStartTime   time.Time
	platform         stdio.Platform
	preRestore       func() HandleStartedFunc
	postRestore      func(*runc.Container, HandleStartedFunc)
	events           chan *v1.ContainerStatus
	checkpointedPIDs map[int]struct{}
	pidsMu           sync.Mutex
	evacuation       sync.Once
	metrics          *v1.ContainerMetrics
	runcVersion      string
	lastConfigReload time.Time
}

func New(ctx context.Context, cfg *v1.Config, r *taskAPI.CreateTaskRequest, pt stdio.Platform, events chan *v1.ContainerStatus) (*Container, error) {
	// get network ns of our container and store it for later use
	netNSPath, err := GetNetworkNS(cfg.Spec)
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
		CheckpointRestore: &sync.Mutex{},
		events:            events,
		checkpointedPIDs:  map[int]struct{}{},
		metrics:           newMetrics(cfg, true),
		runcVersion:       vers.Runc,
	}

	if c.cfg.ReuseportActivator {
		log.G(ctx).Info("using reuseport activator")
		act, err := reuse.New(ctx, c.netNS, c.cfg.Spec.Linux.CgroupsPath, c.activatorOpts(ctx)...)
		if err != nil {
			return nil, err
		}
		c.activator = act
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

	if c.SkipStart() {
		c.setPhaseNotify(v1.ContainerPhase_SCALED_DOWN, 0)
	} else {
		c.setPhaseNotify(v1.ContainerPhase_RUNNING, 0)
	}
	if err := c.initActivator(ctx); err != nil {
		log.G(ctx).Warnf("activator init failed, disabling scale down: %s", err)
		c.cfg.ScaleDownDuration = 0
	}
	if c.SkipStart() {
		if err := c.scaleDown(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (c *Container) Config() *v1.Config {
	return c.cfg
}

func (c *Container) reloadConfig(ctx context.Context) error {
	if c.cfg.LastModified().Equal(c.lastConfigReload) {
		log.G(ctx).Debugf("config file up to date: %s", c.lastConfigReload)
		return nil
	}
	log.G(ctx).Debug("reloading config")
	spec, err := GetSpec(c.Bundle)
	if err != nil {
		return fmt.Errorf("getting container spec: %w", err)
	}
	// copy ports since they might have been discovered on first startup
	ports := c.cfg.Ports
	cfg, err := v1.NewConfig(ctx, spec)
	if err != nil {
		return fmt.Errorf("creating config: %w", err)
	}
	c.cfg = cfg
	if len(c.cfg.Ports) == 0 {
		c.cfg.Ports = ports
	}
	if act, ok := c.activator.(*reuse.Activator); ok {
		if err := act.Reload(c.activatorOpts(ctx)...); err != nil {
			return err
		}
	}
	c.lastConfigReload = c.cfg.LastModified()
	return nil
}

func (c *Container) ScheduleScaleDown() {
	c.scheduleScaleDownIn(c.cfg.ScaleDownDuration)
}

func (c *Container) scheduleScaleDownIn(in time.Duration) {
	// cancel any potential pending scaledonws
	c.CancelScaleDown()

	if in == 0 {
		log.G(c.context).Info("scale down is disabled")
		return
	}

	log.G(c.context).Infof("scheduling scale down in %s", in)
	if c.scaleDownTimer == nil {
		c.scaleDownTimer = time.AfterFunc(in, func() {
			c.scaleDownCheck()
		})
		return
	}
	c.scaleDownTimer.Reset(in)
}

func (c *Container) scaleDownCheck() {
	if !c.activator.Started() {
		c.scaleDownTimer.Reset(c.initRetry())
		log.G(c.context).Infof("activator not ready, delaying scale down by %s", c.initBackoff)
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
			// we reload the config here to ensure ignored addresses are up to
			// date and potentially fix a scaledown issue.
			if err := c.reloadConfig(c.context); err != nil {
				log.G(c.context).WithError(err).Error("reloading config")
			}
			return
		}
	}

	log.G(c.context).Info("scaling down after scale down duration is up")

	if err := c.scaleDown(c.context); err != nil {
		log.G(c.context).Errorf("scale down failed, disabling checkpointing: %s", err)
		c.cfg.DisableCheckpointing = true
		c.scaleDownTimer.Reset(c.cfg.ScaleDownDuration)
	}
}

func (c *Container) CancelScaleDown() {
	if c.scaleDownTimer == nil {
		return
	}
	c.scaleDownTimer.Stop()
}

func (c *Container) setPhase(phase v1.ContainerPhase, duration time.Duration) {
	switch phase {
	case v1.ContainerPhase_RUNNING:
		if duration != 0 {
			c.metrics.LastRestore = timestamppb.Now()
		}
		c.scaledDown = false
		c.metrics.Running = true
	case v1.ContainerPhase_SCALED_DOWN:
		if duration != 0 {
			c.metrics.LastCheckpoint = timestamppb.Now()
		}
		c.scaledDown = true
		c.metrics.Running = false
		if err := c.updateCheckpointMemoryBytes(); err != nil {
			log.G(c.context).WithError(err).Error("updating checkpoint memory metric")
		}
	}
}

func (c *Container) setPhaseNotify(phase v1.ContainerPhase, duration time.Duration) {
	c.setPhase(phase, duration)
	if duration != 0 {
		switch phase {
		case v1.ContainerPhase_RUNNING:
			c.metrics.LastRestoreDuration = durationpb.New(duration)
		case v1.ContainerPhase_SCALED_DOWN:
			c.metrics.LastCheckpointDuration = durationpb.New(duration)
		}
	}
	c.sendEvent(c.Status())
}

func (c *Container) sendFailEvent(phase v1.ContainerPhase, l string) {
	switch phase {
	case v1.ContainerPhase_CHECKPOINT_FAILED:
		c.metrics.CheckpointErrors += 1
	case v1.ContainerPhase_RESTORE_FAILED:
		c.metrics.RestoreErrors += 1
	}
	status := c.Status()
	status.Phase = phase
	status.EventLog = l
	c.sendEvent(status)
}

func (c *Container) SetSkipStart(skip bool) {
	c.skipStart = skip
}

func (c *Container) SkipStart() bool {
	return c.skipStart
}

func (c *Container) Status() *v1.ContainerStatus {
	eventTime := timestamppb.Now()
	phase := v1.ContainerPhase_RUNNING
	if c.metrics.LastRestore != nil {
		eventTime = c.metrics.LastRestore
	}
	eventDuration := c.metrics.LastRestoreDuration
	if c.ScaledDown() {
		phase = v1.ContainerPhase_SCALED_DOWN
		if c.metrics.LastCheckpoint != nil {
			eventTime = c.metrics.LastCheckpoint
		}
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
	c.cleanupImage(ctx)
	_ = c.netNS.Close()
}

func (c *Container) ExitOK(ctx context.Context) {
	if c.drainTimer != nil {
		c.drainTimer.Stop()
		c.drainTimer = nil
	}
	c.Process().SetExited(0)
	c.InitialProcess().SetExited(0)
	c.Stop(ctx)
}

func (c *Container) cleanupImage(ctx context.Context) {
	// with migration, the shim might exit before the image data has been
	// transferred to the new node. The cleanup is the responsibility of the
	// node service.
	if c.cfg.AnyMigrationEnabled() {
		return
	}
	c.deleteImage(ctx)
}

func (c *Container) deleteImage(ctx context.Context) {
	if err := os.RemoveAll(nodev1.ImagePath(c.ID())); err != nil {
		if !os.IsNotExist(err) {
			log.G(ctx).Warnf("unable to cleanup image path: %s", err)
		}
	}
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

func (c *Container) EvacDrainStarted() bool {
	return c.evacDrainStarted.Load()
}

func (c *Container) activatorOpts(ctx context.Context) []reuse.Option {
	opts := []reuse.Option{
		reuse.RestoreHook(c.restoreHandler(c.context)),
		reuse.TrackerIgnoreLocalhost(c.cfg.TrackerIgnoreLocalhost),
	}
	if c.cfg.ProbeAddress != "" {
		addr, err := netip.ParseAddr(c.cfg.ProbeAddress)
		if err != nil {
			log.G(ctx).WithError(err).Warn("invalid probe address configured")
		} else {
			opts = append(opts, reuse.ProbeAddr(&addr))
		}
	}
	if c.cfg.ConnectTimeout > 0 {
		opts = append(opts, reuse.ConnectTimeout(c.cfg.ConnectTimeout))
	}
	return opts
}

func (c *Container) initActivator(ctx context.Context) error {
	c.cancelInit()

	if c.activator == nil && !c.cfg.ReuseportActivator {
		log.G(ctx).Info("using legacy activator")
		act, err := activator.NewServer(ctx, c.netNS, c.detectProbe(ctx), c.restoreHandler(c.context))
		if err != nil {
			return err
		}
		c.activator = act
		if c.cfg.ProxyTimeout > 0 {
			c.activator.SetProxyTimeout(c.cfg.ProxyTimeout)
		}
		if c.cfg.ConnectTimeout > 0 {
			c.activator.SetConnectTimeout(c.cfg.ConnectTimeout)
		}
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
			c.retryInitIn(retryIn)
			return nil
		}

		c.cfg.Ports = ports
	}

	log.G(ctx).Infof("starting activator with ports: %v", c.cfg.Ports)
	if err := c.startActivator(ctx, c.cfg.Ports...); err != nil {
		if errors.Is(err, activator.ErrMapNotFound) || errors.Is(err, activator.ErrNoListeningSockets) {
			c.retryInitIn(c.initRetry())
			return nil
		}
		return err
	}
	return nil
}

// initRetry returns the duration in which the next init should be retried. It
// backs off exponentially with an initial wait of 100 milliseconds.
func (c *Container) initRetry() time.Duration {
	const initial, max = time.Millisecond * 10, time.Minute * 5
	c.initBackoff = min(max, c.initBackoff*2)

	if c.initBackoff == 0 {
		c.initBackoff = initial
	}

	return c.initBackoff
}

func (c *Container) retryInitIn(in time.Duration) {
	log.G(c.context).Infof("scheduling init in %s", in)
	timer := time.AfterFunc(in, func() {
		if err := c.initActivator(c.context); err != nil {
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

// getListeners prepares the activator listeners from a migrated snapshot. It
// first tries to load them from an existing zeropod_listeners.json and if
// that's not present it will call [nodev1.NetinfoBinary] to extract the from
// the criu checkpoint.
func (c *Container) getListeners(ports ...uint16) activator.Listeners {
	if c.cfg.DisableCheckpointing {
		return emptyListeners(ports...)
	}

	lns, err := c.loadListeners()
	if err == nil {
		return lns
	}

	if c.SkipStart() {
		out, err := exec.Command(nodev1.NetinfoBinary, "-id", c.ID()).CombinedOutput()
		if err != nil && !strings.Contains(err.Error(), "no child processes") {
			log.G(c.context).WithError(err).Errorf("calling zeropod-migrate: %s", out)
		} else {
			lns, err := c.loadListeners()
			if err == nil {
				return lns
			}
		}
	}

	return emptyListeners(ports...)
}

func emptyListeners(ports ...uint16) activator.Listeners {
	listeners := activator.Listeners{}
	for _, port := range ports {
		// fallback to just a dual-stack listener for each port. If !skipStart,
		// the listeners will anyways be detected from the app so this is more
		// of a last resort if we skipStart and the listeners from the
		// checkpoint are empty or failed to extract.
		listeners = append(
			listeners,
			activator.Listener{Port: port, Network: activator.NetworkTCPAny},
		)
	}
	return listeners
}

// startActivator starts the activator
func (c *Container) startActivator(ctx context.Context, ports ...uint16) error {
	if c.activator.Started() {
		return nil
	}
	if err := c.activator.AttachExec(); err != nil {
		log.G(ctx).WithError(err).Error("failed to attach activator")
		return err
	}

	if err := c.activator.Start(c.context, c.Pid(), c.getListeners(ports...), c.SkipStart()); err != nil {
		if errors.Is(err, activator.ErrMapNotFound) {
			return err
		}

		log.G(ctx).WithError(err).Error("failed to start activator")
		return err
	}
	log.G(ctx).Printf("activator started")
	return nil
}

func (c *Container) restoreHandler(ctx context.Context) activator.RestoreHook {
	return func() (int, error) {
		restoredContainer, _, err := c.Restore(ctx)
		if err != nil {
			if errors.Is(err, ErrAlreadyRestored) {
				log.G(ctx).Info("container is already restored, ignoring request")
				return c.Pid(), nil
			}
			if errors.Is(err, ErrNoCapacity) {
				log.G(ctx).Info("no capacity to restore, requests are being forwarded")
				return 0, nil
			}
			// restore failed, this is currently unrecoverable, so we set the
			// process to exited and let the runtime recreate it.
			c.Process().SetExited(1)
			c.InitialProcess().SetExited(1)
			log.G(ctx).Errorf("error restoring container, exiting process: %s", err)
		}
		c.Container = restoredContainer
		c.ScheduleScaleDown()
		return c.Pid(), nil
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
			if errors.Is(err, activator.NoActivityRecordedErr{}) {
				continue
			}
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
	c.metrics.CheckpointErrors = 0
	c.metrics.RestoreErrors = 0
}
