package shim

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/icza/backscanner"
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
	c.CheckpointRestore.Lock()
	defer c.CheckpointRestore.Unlock()
	log.G(ctx).Infof("checkpointing is disabled, scaling down by killing")
	c.AddCheckpointedPID(c.Pid())

	// we access the initProcess so we can force delete
	initProcess, ok := c.process.(*process.Init)
	if !ok {
		return fmt.Errorf("process is not of type %T, got %T", process.Init{}, c.process)
	}

	before := time.Now()
	// we simply force-delete the container from runc, which executes a kill
	// signal and then waits until the process is fully stopped.
	if err := initProcess.Runtime().Delete(ctx, c.id, &runcC.DeleteOpts{Force: true}); err != nil {
		return err
	}

	c.setPhaseNotify(v1.ContainerPhase_SCALED_DOWN, time.Since(before))
	return nil
}

func (c *Container) checkpoint(ctx context.Context) error {
	c.CheckpointRestore.Lock()
	defer c.CheckpointRestore.Unlock()

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

	resetOnErr := func() {
		c.DeleteCheckpointedPID(c.Pid())
		_ = c.activator.DisableRedirects()
		lines := printCriuLogs(ctx, filepath.Join(workDir, "dump.log"))
		c.sendFailEvent(v1.ContainerPhase_CHECKPOINT_FAILED, lines)
	}

	if c.cfg.PreDump {
		// for the pre-dump we set the ImagePath to be a sub-path of our container image path
		opts.ImagePath = nodev1.PreDumpDir(c.ID())

		beforePreDump := time.Now()
		if err := initProcess.Runtime().Checkpoint(ctx, c.ID(), opts, runcC.PreDump); err != nil {
			log.G(ctx).Errorf("error pre-dumping container: %s", err)
			resetOnErr()
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
		resetOnErr()
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

func printCriuLogs(ctx context.Context, file string) string {
	lines, err := getLastLines(ctx, file, 20)
	if err != nil {
		log.G(ctx).Errorf("error reading criu log: %s", err)
	}
	log.G(ctx).Errorf("last 20 lines of criu log: %s", lines)
	return lines
}

func getLastLines(ctx context.Context, filepath string, num int) (string, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	stat, err := os.Stat(filepath)
	if err != nil {
		return "", err
	}
	lines := make([]string, 0, num)
	scanner := backscanner.New(file, int(stat.Size()))
	linesRead := 0
	for {
		line, _, err := scanner.Line()
		if err != nil {
			if err == io.EOF {
				return strings.Join(lines, `\n`), nil
			}
			return "", fmt.Errorf("reading log lines: %w", err)
		}
		lines = slices.Insert(lines, 0, line)
		if linesRead >= num {
			break
		}
		linesRead++
	}
	return strings.Join(lines, "\n"), nil
}
