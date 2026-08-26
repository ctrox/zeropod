package shim

import (
	"os"
	"path/filepath"

	"github.com/checkpoint-restore/go-criu/v8/stats"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"golang.org/x/sys/unix"
)

func newMetrics(cfg *v1.Config, running bool) *v1.ContainerMetrics {
	return &v1.ContainerMetrics{
		Name:         cfg.ContainerName,
		PodName:      cfg.PodName,
		PodNamespace: cfg.PodNamespace,
		Running:      running,
		DryRun:       cfg.DryRun,
	}
}

func (c *Container) updateCheckpointMemoryBytes() error {
	if _, err := os.Stat(filepath.Join(nodev1.WorkDirPath(c.ID()), stats.StatsDump)); err == nil {
		// move stats file to the snapshot path so it gets migrated
		err := os.Rename(
			filepath.Join(nodev1.WorkDirPath(c.ID()), stats.StatsDump),
			filepath.Join(nodev1.SnapshotPath(c.ID()), stats.StatsDump),
		)
		if err != nil {
			return err
		}
	}
	imgDir, err := os.Open(nodev1.SnapshotPath(c.ID()))
	if err != nil {
		return err
	}
	dumpStats, err := stats.CriuGetDumpStats(imgDir)
	if err != nil {
		return nil
	}
	c.metrics.CheckpointMemoryBytes = dumpStats.GetPagesWritten() * uint64(unix.Getpagesize())
	return nil
}
