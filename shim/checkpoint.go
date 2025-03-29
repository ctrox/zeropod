package shim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/ctrox/zeropod/activator"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

const retryInterval = time.Second

func (c *Container) scaleDown(ctx context.Context) error {
	if err := c.startActivator(ctx); err != nil {
		if errors.Is(err, errNoPortsDetected) {
			log.G(ctx).Infof("no ports detected, rescheduling scale down in %s", retryInterval)
			return c.scheduleScaleDownIn(retryInterval)
		}

		if errors.Is(err, activator.ErrMapNotFound) {
			log.G(ctx).Infof("activator is not ready, rescheduling scale down in %s", retryInterval)
			return c.scheduleScaleDownIn(retryInterval)
		}

		return err
	}

	if err := c.activator.Reset(); err != nil {
		return err
	}

	if err := c.tracker.RemovePid(uint32(c.process.Pid())); err != nil {
		// key could not exist, just log the error for now
		log.G(ctx).Errorf("unable to remove pid %d: %s", c.process.Pid(), err)
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

func (c *Container) kill(ctx context.Context) error {
	c.checkpointRestore.Lock()
	defer c.checkpointRestore.Unlock()
	log.G(ctx).Infof("checkpointing is disabled, scaling down by killing")
	c.AddCheckpointedPID(c.Pid())

	if err := c.process.Kill(ctx, 9, false); err != nil {
		return err
	}
	c.SetScaledDown(true)
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
		AllowOpenTCP:             true,
		AllowExternalUnixSockets: true,
		AllowTerminal:            false,
		FileLocks:                true,
		EmptyNamespaces:          []string{},
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

	c.SetScaledDown(true)
	c.metrics.LastCheckpointDuration = durationpb.New(time.Since(beforeCheckpoint))
	log.G(ctx).Infof("checkpointing done in %s", time.Since(beforeCheckpoint))

	return nil
}
