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
	"github.com/containerd/containerd/runtime/v2/runc"
	runcC "github.com/containerd/go-runc"
)

const retryInterval = time.Second

func (c *Container) scaleDown(ctx context.Context, container *runc.Container, p process.Process) error {
	if err := c.initActivator(ctx); err != nil {
		if errors.Is(err, errNoPortsDetected) {
			log.G(ctx).Infof("no ports detected, rescheduling scale down in %s", retryInterval)
			return c.scheduleScaleDownIn(container, retryInterval)
		}
		return err
	}

	if err := c.tracker.RemovePid(uint32(p.Pid())); err != nil {
		// key could not exist, just log the error for now
		log.G(ctx).Errorf("unable to remove pid %d: %s", p.Pid(), err)
	}

	if c.cfg.DisableCheckpointing {
		log.G(ctx).Infof("checkpointing is disabled, scaling down by killing")

		c.SetScaledDown(true)
		if err := p.Kill(ctx, 9, false); err != nil {
			return err
		}
	} else {
		if err := c.checkpoint(ctx, container, p); err != nil {
			return err
		}

	}

	beforeActivator := time.Now()
	if err := c.startActivator(ctx, container); err != nil {
		log.G(ctx).Errorf("unable to start zeropod: %s", err)
		return err
	}

	if c.cfg.DisableCheckpointing {
		log.G(ctx).Infof("activator started in %s", time.Since(beforeActivator))
		return nil
	}

	// after checkpointing is done and we have started our activator in place
	// of the real process' socket, we can unlock the network again.
	if err := c.netLocker.Unlock(c.cfg.Ports); err != nil {
		log.G(ctx).Errorf("unable to remove nftable: %s", err)
		return err
	}

	log.G(ctx).Infof("activator started and net-lock removed in %s", time.Since(beforeActivator))
	return nil
}

func (c *Container) checkpoint(ctx context.Context, container *runc.Container, p process.Process) error {
	c.checkpointRestore.Lock()
	defer c.checkpointRestore.Unlock()

	snapshotDir := snapshotDir(container.Bundle)

	if err := os.RemoveAll(snapshotDir); err != nil {
		return fmt.Errorf("unable to prepare snapshot dir: %w", err)
	}

	workDir := path.Join(snapshotDir, "work")
	log.G(ctx).Infof("checkpointing process %d of container to %s", p.Pid(), snapshotDir)

	initProcess, ok := p.(*process.Init)
	if !ok {
		return fmt.Errorf("process is not of type %T, got %T", process.Init{}, p)
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
		opts.ImagePath = preDumpDir(container.Bundle)

		beforePreDump := time.Now()
		if err := initProcess.Runtime().Checkpoint(ctx, container.ID, opts, runcC.PreDump); err != nil {
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

	// before executing the checkpoint we want to lock the network so that
	// incoming connections during the checkpoint will be retried (on a TCP
	// level since we drop the traffic).
	if err := c.netLocker.Lock(c.cfg.Ports); err != nil {
		return fmt.Errorf("unable to lock network: %w", err)
	}

	// TODO: as a result of the IP/NF tables rules we sometimes get > 1s
	// delays when the client is connecting during checkpointing. This can be
	// reproduced easily by running the benchmark without any sleeps. This is
	// most probably caused by TCP SYN retransmissions:
	// $ netstat -s | grep -i retrans
	// 3 segments retransmitted
	// TCPSynRetrans: 3
	// Not sure if we can even do something about this as the issue is on the
	// client side.
	// https://arthurchiao.art/blog/customize-tcp-initial-rto-with-bpf/#tl-dr

	c.SetScaledDown(true)

	if c.cfg.PreDump {
		// ParentPath is the relative path from the ImagePath to the pre-dump dir.
		opts.ParentPath = relativePreDumpDir()
	}

	// ImagePath is always the same, regardless of pre-dump
	opts.ImagePath = containerDir(container.Bundle)

	beforeCheckpoint := time.Now()
	if err := initProcess.Runtime().Checkpoint(ctx, container.ID, opts); err != nil {
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
