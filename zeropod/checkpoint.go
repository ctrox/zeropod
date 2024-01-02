package zeropod

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/process"
	runcC "github.com/containerd/go-runc"
)

const retryInterval = time.Second

func (c *Container) scaleDown(ctx context.Context) error {
	if err := c.initActivator(ctx); err != nil {
		if errors.Is(err, errNoPortsDetected) {
			log.G(ctx).Infof("no ports detected, rescheduling scale down in %s", retryInterval)
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

	if c.cfg.DisableCheckpointing {
		log.G(ctx).Infof("checkpointing is disabled, scaling down by killing")

		c.SetScaledDown(true)
		if err := c.process.Kill(ctx, 9, false); err != nil {
			return err
		}
	} else {
		if err := c.checkpoint(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (c *Container) checkpoint(ctx context.Context) error {
	c.checkpointRestore.Lock()
	defer c.checkpointRestore.Unlock()

	snapshotDir := snapshotDir(c.Bundle)

	if err := os.RemoveAll(snapshotDir); err != nil {
		return fmt.Errorf("unable to prepare snapshot dir: %w", err)
	}

	workDir := path.Join(snapshotDir, "work")
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
		opts.ImagePath = preDumpDir(c.Bundle)

		beforePreDump := time.Now()
		if err := initProcess.Runtime().Checkpoint(ctx, c.ID(), opts, runcC.PreDump); err != nil {
			c.SetScaledDown(false)

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

	c.SetScaledDown(true)

	if c.cfg.PreDump {
		// ParentPath is the relative path from the ImagePath to the pre-dump dir.
		opts.ParentPath = relativePreDumpDir()
	}

	// ImagePath is always the same, regardless of pre-dump
	opts.ImagePath = containerDir(c.Bundle)

	beforeCheckpoint := time.Now()
	if err := initProcess.Runtime().Checkpoint(ctx, c.ID(), opts); err != nil {
		c.SetScaledDown(false)

		log.G(ctx).Errorf("error checkpointing container: %s", err)
		b, err := os.ReadFile(path.Join(workDir, "dump.log"))
		if err != nil {
			log.G(ctx).Errorf("error reading dump.log: %s", err)
		}
		log.G(ctx).Errorf("dump.log: %s", b)
		return err
	}

	checkpointDuration.With(c.labels()).Observe(time.Since(beforeCheckpoint).Seconds())
	log.G(ctx).Infof("checkpointing done in %s", time.Since(beforeCheckpoint))

	return nil
}
