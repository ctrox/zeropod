package shim

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/log"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
)

const (
	concurrentStreams       = 4
	streamingDumpTimeout    = time.Minute
	streamingRestoreTimeout = time.Minute
)

func shardFds(numFds int) string {
	fds := make([]string, 0, numFds)
	for i := range numFds {
		fds = append(fds, strconv.Itoa(i+3))
	}
	return strings.Join(fds, ",")
}

func (c *Container) prepareStreamingDump(ctx context.Context) error {
	beforeLazy := time.Now()
	os.MkdirAll(nodev1.SnapshotPath(c.ID()), os.ModePerm)
	socketInit := make(chan struct{})
	errCh := make(chan error)
	go func() {
		c.streamingCR.Lock()
		defer c.streamingCR.Unlock()
		streamer := exec.CommandContext(ctx, "criu-image-streamer",
			"--images-dir", nodev1.SnapshotPath(c.ID()),
			"--shard-fds", shardFds(concurrentStreams),
			"capture",
		)
		pipes := []*os.File{}
		for i := range concurrentStreams {
			lz4 := exec.CommandContext(
				ctx, "lz4", "-f", "-",
				filepath.Join(nodev1.SnapshotPath(c.ID()), fmt.Sprintf("img-%d.lz4", i)),
			)
			r, w, err := os.Pipe()
			if err != nil {
				log.G(ctx).WithError(err).Error("error creating pipe", i)
				return
			}
			pipes = append(pipes, r, w)
			lz4.Stdin = r
			if err := lz4.Start(); err != nil {
				log.G(ctx).WithError(err).Error("error running lz4")
			}
			streamer.ExtraFiles = append(streamer.ExtraFiles, w)
			go func() {
				if err := lz4.Wait(); err != nil {
					log.G(ctx).WithError(err).Debug("waiting for lz4")
				}
			}()
		}
		stderr, err := streamer.StderrPipe()
		if err != nil {
			log.G(ctx).WithError(err).Error("creating criu-image-streamer stderr pipe")
			return
		}
		scanner := bufio.NewScanner(stderr)
		log.G(ctx).WithField("cmd", streamer.Args).Debug("starting criu-image-streamer")
		if err := streamer.Start(); err != nil {
			log.G(ctx).WithError(err).Error("error running criu-image-streamer")
			return
		}
		for scanner.Scan() {
			if scanner.Text() == "socket-init" {
				socketInit <- struct{}{}
				break
			}
			if strings.Contains(scanner.Text(), "Error") {
				errCh <- fmt.Errorf("unexpected output from criu-image-streamer: %s", scanner.Text())
				break
			}
		}
		if err := streamer.Wait(); err != nil {
			log.G(ctx).WithError(err).Debug("waiting for criu-image-streamer")
		}
		log.G(ctx).Info("criu-image-streamer closed")
		for _, pipe := range pipes {
			pipe.Close()
		}
	}()
	select {
	case <-socketInit:
		log.G(ctx).WithField("duration", time.Since(beforeLazy).String()).Info("criu-image-streamer daemon started")
		return nil
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readerFile(r io.Reader) (*os.File, error) {
	reader, writer, err := os.Pipe()

	if err != nil {
		return nil, err
	}

	go func() {
		io.Copy(writer, r)
		writer.Close()
	}()

	return reader, nil
}

func (c *Container) prepareStreamingRestore(ctx context.Context) error {
	beforeLazy := time.Now()
	os.MkdirAll(nodev1.SnapshotPath(c.ID()), os.ModePerm)
	socketInit := make(chan struct{})
	errCh := make(chan error)
	go func() {
		c.streamingCR.Lock()
		defer c.streamingCR.Unlock()
		streamer := exec.CommandContext(ctx, "criu-image-streamer",
			"--images-dir", nodev1.SnapshotPath(c.ID()),
			"--shard-fds", shardFds(concurrentStreams),
			"serve",
		)
		pipes := []*os.File{}
		for i := range concurrentStreams {
			lz4 := exec.CommandContext(
				ctx, "lz4", "-d",
				filepath.Join(nodev1.SnapshotPath(c.ID()), fmt.Sprintf("img-%d.lz4", i)),
				"-",
			)
			r, w, err := os.Pipe()
			if err != nil {
				log.G(ctx).WithError(err).Error("error creating pipe", i)
				return
			}
			pipes = append(pipes, r, w)
			lz4.Stdout = w
			if err := lz4.Start(); err != nil {
				log.G(ctx).WithError(err).Error("error running lz4")
			}
			streamer.ExtraFiles = append(streamer.ExtraFiles, r)
			go func() {
				if err := lz4.Wait(); err != nil {
					log.G(ctx).WithError(err).Debug("waiting for lz4")
				}
			}()
		}
		stderr, err := streamer.StderrPipe()
		if err != nil {
			log.G(ctx).WithError(err).Error("creating criu-image-streamer stderr pipe")
			return
		}
		scanner := bufio.NewScanner(stderr)
		log.G(ctx).WithField("cmd", streamer.Args).Debug("starting criu-image-streamer")
		if err := streamer.Start(); err != nil {
			log.G(ctx).WithError(err).Error("error running criu-image-streamer")
			return
		}
		for _, pipe := range pipes {
			pipe.Close()
		}
		for scanner.Scan() {
			if scanner.Text() == "socket-init" {
				socketInit <- struct{}{}
				break
			}
			if strings.Contains(scanner.Text(), "Error") {
				errCh <- fmt.Errorf("unexpected output from criu-image-streamer: %s", scanner.Text())
				break
			}
		}
		if err := streamer.Wait(); err != nil {
			log.G(ctx).WithError(err).Debug("waiting for criu-image-streamer")
		}
		log.G(ctx).Info("criu-image-streamer closed")
	}()
	select {
	case <-socketInit:
		log.G(ctx).WithField("duration", time.Since(beforeLazy).String()).Info("criu-image-streamer daemon started")
		return nil
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
