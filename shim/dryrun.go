package shim

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/containerd/log"
	"github.com/ctrox/zeropod/activator"
)

// dryRunPollInterval is how often we recheck activity while in a simulated
// scaled-down state, to detect a would-be restore.
const dryRunPollInterval = time.Second

// dryRunScaleDown simulates a scale down: it does not checkpoint/kill the
// process and does not enable the eBPF redirect, so the real process keeps
// serving traffic uninterrupted. It logs and starts polling for activity so
// it can log a "would have restored" once traffic resumes.
func (c *Container) dryRunScaleDown(ctx context.Context) error {
	if c.dryRunScaledDown {
		return nil
	}
	c.dryRunScaledDown = true
	c.dryRunSince = time.Now()
	log.G(ctx).Infof("dry-run: would have scaled down container %s after %s of inactivity", c.ID(), c.cfg.ScaleDownDuration)
	c.sendDryRunEvent(fmt.Sprintf("would have scaled down after %s of inactivity", c.cfg.ScaleDownDuration))
	c.scheduleDryRunRestoreCheck()
	return nil
}

func (c *Container) scheduleDryRunRestoreCheck() {
	if c.dryRunPollTimer == nil {
		c.dryRunPollTimer = time.AfterFunc(dryRunPollInterval, c.dryRunRestoreCheck)
		return
	}
	c.dryRunPollTimer.Reset(dryRunPollInterval)
}

func (c *Container) dryRunRestoreCheck() {
	last, err := c.lastActivity()
	if err != nil && !errors.Is(err, activator.NoActivityRecordedErr{}) {
		log.G(c.context).Warnf("dry-run: unable to get last TCP activity: %s", err)
	}
	if err == nil && last.After(c.dryRunSince) {
		// activity-triggered restore mirrors the real network-triggered
		// restore (activator restoreHandler), which reschedules immediately.
		c.dryRunRestore(c.context, fmt.Sprintf("last activity %s ago", time.Since(last)), true)
		return
	}
	c.scheduleDryRunRestoreCheck()
}

// DryRunExec simulates a restore triggered by kubectl exec, mirroring the
// real restore-on-exec behaviour (see task.wrapper.Exec) without touching the
// real process. No-op unless dry-run is currently simulating a scaled down
// state. Like the real exec path, it deliberately does not reschedule the
// scale-down timer itself: that is left to wrapper.Delete, which only
// reschedules once all running execs for the container have ended, so a
// long-running exec session (e.g. an interactive shell) keeps suppressing
// (simulated) scale-down for its entire duration, not just at the moment
// exec was called.
func (c *Container) DryRunExec(ctx context.Context) {
	if !c.cfg.DryRun || !c.dryRunScaledDown {
		return
	}
	c.dryRunRestore(ctx, "got exec", false)
}

// dryRunRestore logs a simulated restore and clears the simulated scaled-down
// state. If reschedule is true, it also resumes the normal scale-down timer
// cycle immediately; otherwise the caller is responsible for rescheduling
// once appropriate.
func (c *Container) dryRunRestore(ctx context.Context, reason string, reschedule bool) {
	log.G(ctx).Infof("dry-run: would have restored container %s, %s", c.ID(), reason)
	c.sendDryRunEvent(fmt.Sprintf("would have restored (%s)", reason))
	c.dryRunScaledDown = false
	c.cancelDryRunPoll()
	if reschedule {
		c.ScheduleScaleDown()
	}
}

// cancelDryRunPoll stops the dry-run restore-check poller, if running. Must
// be called on container shutdown to avoid leaking the timer goroutine.
func (c *Container) cancelDryRunPoll() {
	if c.dryRunPollTimer == nil {
		return
	}
	c.dryRunPollTimer.Stop()
}
