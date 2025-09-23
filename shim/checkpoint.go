package shim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/ctrox/zeropod/activator"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
)

func (c *Container) scaleDown(ctx context.Context) error {
	if err := c.startActivator(ctx); err != nil {
		if errors.Is(err, errNoPortsDetected) {
			retryIn := c.scaleDownRetry()
			log.G(ctx).Infof("no ports detected, rescheduling scale down in %s", retryIn)
			return c.scheduleScaleDownIn(retryIn)
		}

		if errors.Is(err, activator.ErrMapNotFound) {
			retryIn := c.scaleDownRetry()
			log.G(ctx).Infof("activator is not ready, rescheduling scale down in %s", retryIn)
			return c.scheduleScaleDownIn(retryIn)
		}

		return err
	}

	if err := c.activator.Reset(); err != nil {
		return err
	}

	if err := c.tracker.RemovePid(uint32(c.process.Pid())); err != nil {
		// key could not exist, just log the error for now
		log.G(ctx).Warnf("unable to remove pid %d: %s", c.process.Pid(), err)
	}

	if c.ScaledDown() {
		return nil
	}

	if c.cfg.DisableCheckpointing {
		if err := c.kill(ctx); err != nil {
			return err
		}
		return nil
	}

	if err := c.checkpoint(ctx); err != nil {
		return err
	}

	return nil
}

// scaleDownRetry returns the duration in which the next scaledown should be
// retried. It backs off exponentially with an initial wait of 1 second.
func (c *Container) scaleDownRetry() time.Duration {
	const initial, max = time.Second, time.Minute * 5
	c.scaleDownBackoff = c.scaleDownBackoff * 2
	if c.scaleDownBackoff >= max {
		c.scaleDownBackoff = max
	}

	if c.scaleDownBackoff == 0 {
		c.scaleDownBackoff = initial
	}

	return c.scaleDownBackoff
}

func (c *Container) kill(ctx context.Context) error {
	c.checkpointRestore.Lock()
	defer c.checkpointRestore.Unlock()
	log.G(ctx).Infof("checkpointing is disabled, scaling down by killing")
	c.AddCheckpointedPID(c.Pid())

	if err := c.process.Kill(ctx, 9, false); err != nil {
		return err
	}
	c.setPhaseNotify(v1.ContainerPhase_SCALED_DOWN, 0)
	return nil
}

func (c *Container) checkpoint(ctx context.Context) error {
	c.checkpointRestore.Lock()
	defer c.checkpointRestore.Unlock()

	snapshotDir := nodev1.SnapshotPath(c.ID())
	if err := os.RemoveAll(snapshotDir); err != nil {
		return fmt.Errorf("unable to prepare snapshot dir: %w", err)
	}

	workDir := nodev1.WorkDirPath(c.ID())
	log.G(ctx).Infof("checkpointing process %d of container to %s", c.process.Pid(), snapshotDir)

	initProcess, ok := c.process.(*process.Init)
	if !ok {
		return fmt.Errorf("process is not of type %T, got %T", process.Init{}, c.process)
	}

	opts := &runcC.CheckpointOpts{
		WorkDir:                  workDir,
		AllowOpenTCP:             false,
		AllowExternalUnixSockets: true,
		AllowTerminal:            false,
		FileLocks:                true,
		EmptyNamespaces:          []string{},
		ExtraArgs:                c.checkpointExtraArgs(),
	}

	if c.cfg.PreDump {
		// for the pre-dump we set the ImagePath to be a sub-path of our container image path
		opts.ImagePath = nodev1.PreDumpDir(c.ID())

		beforePreDump := time.Now()
		if err := initProcess.Runtime().Checkpoint(ctx, c.ID(), opts, runcC.PreDump); err != nil {
			log.G(ctx).Errorf("error pre-dumping container: %s", err)
			b, err := os.ReadFile(path.Join(workDir, "dump.log"))
			if err != nil {
				log.G(ctx).Errorf("error reading dump.log: %s", err)
			}
			log.G(ctx).Errorf("dump.log: %s", b)
			return err
		}

		log.G(ctx).Infof("pre-dumping done in %s", time.Since(beforePreDump))
	}

	if c.cfg.PreDump {
		// ParentPath is the relative path from the ImagePath to the pre-dump dir.
		opts.ParentPath = nodev1.RelativePreDumpDir()
	}

	c.AddCheckpointedPID(c.Pid())
	// ImagePath is always the same, regardless of pre-dump
	opts.ImagePath = nodev1.SnapshotPath(c.ID())

	beforeCheckpoint := time.Now()
	if err := initProcess.Runtime().Checkpoint(ctx, c.ID(), opts); err != nil {
		log.G(ctx).Errorf("error checkpointing container: %s", err)
		b, err := os.ReadFile(path.Join(workDir, "dump.log"))
		if err != nil {
			log.G(ctx).Errorf("error reading dump.log: %s", err)
		}
		log.G(ctx).Errorf("dump.log: %s", b)
		return err
	}

	c.setPhaseNotify(v1.ContainerPhase_SCALED_DOWN, time.Since(beforeCheckpoint))
	log.G(ctx).Infof("checkpointing done in %s", c.metrics.LastCheckpointDuration.AsDuration())

	return nil
}

const checkpointArgSkipTCPInFlight = "--tcp-skip-in-flight"

func (c *Container) checkpointExtraArgs() []string {
	def := []string{}
	spl := strings.Split(c.runcVersion, ".")
	if len(spl) < 2 {
		return def
	}
	maj, err := strconv.Atoi(spl[0])
	if err != nil {
		return def
	}
	min, err := strconv.Atoi(spl[1])
	if err != nil {
		return def
	}
	if maj == 1 && min >= 3 || maj > 1 {
		return []string{checkpointArgSkipTCPInFlight}
	}
	return def
}
