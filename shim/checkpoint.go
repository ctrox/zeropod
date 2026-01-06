package shim

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
)

func (c *Container) scaleDown(ctx context.Context) error {
	if c.ScaledDown() {
		return nil
	}

	if err := c.activator.Reset(); err != nil {
		return err
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
		def = append(def, checkpointArgSkipTCPInFlight)
	}

	// Check if the cuda-checkpoint plugin is installed and add it to the library path
	if exe, err := os.Executable(); err == nil {
		// resolve plugin path relative to the shim binary
		// shim is in .../bin/shim, plugin is in .../lib/criu/cuda.so
		cudaPlugin := path.Join(path.Dir(path.Dir(exe)), "lib", "criu", "cuda.so")
		if _, err := os.Stat(cudaPlugin); err == nil {
			def = append(def, "--lib", cudaPlugin)
		} else {
			// fallback to default path
			cudaPlugin = "/opt/zeropod/lib/criu/cuda.so"
			if _, err := os.Stat(cudaPlugin); err == nil {
				def = append(def, "--lib", cudaPlugin)
			}
		}
	}

	return def
}
