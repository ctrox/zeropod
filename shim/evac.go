package shim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/process"
	runcC "github.com/containerd/go-runc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/prometheus/procfs"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const evacTimeout = time.Second * 10

func (c *Container) MigrationEnabled() bool {
	return c.cfg.AnyMigrationEnabled()
}

func (c *Container) Evac(ctx context.Context, scaledDown bool) error {
	var err error
	c.evacuation.Do(func() {
		if scaledDown {
			err = c.evacScaledDown(ctx)
			return
		}

		if !c.cfg.LiveMigrationEnabled() {
			log.G(ctx).Debug("live migration is not enabled, aborting evac")
			return
		}

		defer cleanupEvacFiles(ctx, c.ID())
		err = c.evac(ctx)
		if err != nil {
			b, err := os.ReadFile(filepath.Join(nodev1.WorkDirPath(c.ID()), "dump.log"))
			if err != nil {
				log.G(ctx).Errorf("error reading dump.log: %s", err)
			}
			log.G(ctx).Errorf("dump.log: %s", b)
		}
	})
	return err
}

func (c *Container) evac(ctx context.Context) error {
	conn, err := net.Dial("unix", nodev1.SocketPath)
	if err != nil {
		return fmt.Errorf("dialing node service: %w", err)
	}
	defer conn.Close()

	evacReq := &nodev1.EvacRequest{
		PodInfo: &nodev1.PodInfo{
			Name:          c.cfg.PodName,
			Namespace:     c.cfg.PodNamespace,
			ContainerName: c.cfg.ContainerName,
		},
		MigrationInfo: &nodev1.MigrationInfo{
			LiveMigration: true,
			ImageId:       c.ID(),
		},
	}
	log.G(ctx).Info("making evac preparation request")
	nodeClient := nodev1.NewNodeClient(ttrpc.NewClient(conn))
	if _, err := nodeClient.PrepareEvac(ctx, evacReq); err != nil {
		return fmt.Errorf("requesting evac: %w", err)
	}

	initProcess, ok := c.process.(*process.Init)
	if !ok {
		return fmt.Errorf("process is not of type %T, got %T", process.Init{}, c.process)
	}

	nodeImagePath := nodev1.ImagePath(c.ID())
	if err := os.MkdirAll(nodeImagePath, os.ModePerm); err != nil {
		return fmt.Errorf("creating images path: %w", err)
	}

	log.G(ctx).Infof("checkpointing process %d of container to %s", c.process.Pid(), nodev1.SnapshotPath(c.ID()))

	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("unable to create pipe: %w", err)
	}
	errChan := make(chan error, 1)
	lazyStarted := make(chan bool, 1)
	done := make(chan bool, 1)
	checkpointCtx, cancelCheckpoint := context.WithCancel(ctx)
	defer cancelCheckpoint()
	var pausedAt time.Time
	go waitForStatus(ctx, lazyStarted, errChan, statusRead)
	go func(errChan chan error) {
		pausedAt = time.Now()
		if err := initProcess.Runtime().Checkpoint(
			checkpointCtx, c.ID(), &runcC.CheckpointOpts{
				ImagePath:                nodev1.SnapshotPath(c.ID()),
				CriuPageServer:           fmt.Sprintf("%s:12345", nodev1.LazyPagesSocket(c.ID())),
				AllowOpenTCP:             false,
				AllowExternalUnixSockets: true,
				AllowTerminal:            false,
				FileLocks:                true,
				EmptyNamespaces:          []string{},
				WorkDir:                  nodev1.WorkDirPath(c.ID()),
				LazyPages:                true,
				StatusFile:               statusWrite,
				ExtraArgs:                c.checkpointExtraArgs(),
			}); err != nil {
			errChan <- err
			return
		}
		log.G(ctx).Info("done checkpointing")
		done <- true
	}(errChan)
	for {
		select {
		case <-time.After(evacTimeout):
			cancelCheckpoint()
			// unfortunately, cancelling the context does not really abort the
			// lazy checkpointing. To do that we simply connect to the page
			// server and send some garbage data so it will cancel.
			// TODO: maybe there is some graceful abort command we can send to the page server?
			conn, err := net.Dial("unix", nodev1.LazyPagesSocket(c.ID()))
			if err != nil {
				return fmt.Errorf("timeout checkpointing container: dialing page server: %w", err)
			}
			defer conn.Close()
			if _, err := conn.Write([]byte("abort")); err != nil {
				return fmt.Errorf("timeout checkpointing container: writing to page server: %w", err)
			}

			return fmt.Errorf("timeout checkpointing container")
		case err := <-errChan:
			return fmt.Errorf("failed checkpointing: %w", err)
		case <-lazyStarted:
			log.G(ctx).Info("successful started lazy checkpointing")
			c.prepareMigrateData(ctx)

			log.G(ctx).Infof("making evac request with image ID: %s", c.ID())
			evacReq.MigrationInfo.PausedAt = timestamppb.New(pausedAt)
			if _, err := nodeClient.Evac(ctx, evacReq); err != nil {
				return fmt.Errorf("requesting evac: %w", err)
			}
		case <-done:
			// once checkpointed, consider the container scaled down to ensure
			// proper exit cleanup
			c.setPhase(v1.ContainerPhase_SCALED_DOWN)
			log.G(ctx).Info("done case")
			return nil
		}
	}
}

func cleanupEvacFiles(ctx context.Context, containerID string) {
	if err := os.Remove(nodev1.LazyPagesSocket(containerID)); err != nil {
		log.G(ctx).WithError(err).Warn("cleaning up lazy pages socket")
	}
	if err := os.RemoveAll(nodev1.ImagePath(containerID)); err != nil {
		log.G(ctx).WithError(err).Warn("cleaning up checkpoint image")
	}
}

func waitForStatus(ctx context.Context, done chan bool, errChan chan error, status *os.File) {
	log.G(ctx).Info("reading status file")
	data := make([]byte, 1)
	if _, err := status.Read(data); err != nil {
		if err != io.EOF {
			errChan <- err
		}
	}
	log.G(ctx).Infof("read something: %s, we are done", data)
	done <- true
}

func (c *Container) evacScaledDown(ctx context.Context) error {
	conn, err := net.Dial("unix", nodev1.SocketPath)
	if err != nil {
		return fmt.Errorf("dialing node service: %w", err)
	}
	defer conn.Close()

	ports := []int32{}
	for _, p := range c.cfg.Ports {
		ports = append(ports, int32(p))
	}
	evacReq := &nodev1.EvacRequest{
		PodInfo: &nodev1.PodInfo{
			Name:          c.cfg.PodName,
			Namespace:     c.cfg.PodNamespace,
			ContainerName: c.cfg.ContainerName,
			Ports:         ports,
		},
		MigrationInfo: &nodev1.MigrationInfo{
			LiveMigration: false,
			ImageId:       c.ID(),
			PausedAt:      timestamppb.Now(),
		},
	}
	log.G(ctx).Info("making evac preparation request")
	nodeClient := nodev1.NewNodeClient(ttrpc.NewClient(conn))
	if _, err := nodeClient.PrepareEvac(ctx, evacReq); err != nil {
		return fmt.Errorf("requesting evac: %w", err)
	}
	c.prepareMigrateData(ctx)

	if _, err := nodeClient.Evac(ctx, evacReq); err != nil {
		return fmt.Errorf("requesting evac: %w", err)
	}
	return nil
}

func (c *Container) prepareMigrateData(ctx context.Context) {
	if c.cfg.DisableMigrateData {
		return
	}
	// for now we allow this to fail and continue with the migration since
	// it could be unreliable and is not strictly required to migrate
	// (although this depends on the application).
	if err := moveUpperDirToImage(c.ID()); err != nil {
		log.G(ctx).Errorf("adding container data to image: %s", err)
	}
}

// moveUpperDirToImage finds the overlayfs upper directory of the container and
// moves it to the snapshot upper dir to be transferred by the evac. Only call
// this once the container is either stopped or frozen.
func moveUpperDirToImage(containerID string) error {
	upper, err := findUpperDir(containerID)
	if err != nil {
		return fmt.Errorf("finding upper storage dir: %w", err)
	}
	to := nodev1.UpperPath(containerID)
	if err := os.MkdirAll(to, 0644); err != nil {
		return err
	}

	return renameAllSubDirs(upper, to)
}

// MoveImageToUpperDir does the same as moveUpperDirToImage but in reverse
func MoveImageToUpperDir(containerID, imageDir string) error {
	upper, err := findUpperDir(containerID)
	if err != nil {
		return fmt.Errorf("finding upper storage dir: %w", err)
	}

	return renameAllSubDirs(filepath.Join(imageDir, nodev1.UpperSuffix), upper)
}

func renameAllSubDirs(from, to string) error {
	dirs, err := os.ReadDir(from)
	if err != nil {
		return err
	}

	var errs []error
	for _, dir := range dirs {
		errs = append(errs, os.Rename(filepath.Join(from, dir.Name()), filepath.Join(to, dir.Name())))
	}

	return errors.Join(errs...)
}

func findUpperDir(containerID string) (string, error) {
	p, err := procfs.NewFS("/proc")
	if err != nil {
		return "", fmt.Errorf("new procfs: %w", err)
	}

	proc, err := p.Self()
	if err != nil {
		return "", fmt.Errorf("getting process info: %w", err)
	}

	mountInfo, err := proc.MountInfo()
	if err != nil {
		return "", fmt.Errorf("getting mount info: %w", err)
	}

	for _, mount := range mountInfo {
		if strings.Contains(mount.MountPoint, containerID) {
			if v, ok := mount.SuperOptions["upperdir"]; ok {
				return v, nil
			}
		}
	}

	return "", fmt.Errorf("upper dir not found for container %s", containerID)
}
