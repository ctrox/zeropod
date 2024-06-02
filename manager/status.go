package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/containerd/ttrpc"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/runc/task"
	"github.com/fsnotify/fsnotify"
	"google.golang.org/protobuf/types/known/emptypb"
)

type StatusHandler interface {
	Handle(context.Context, *v1.ContainerStatus) error
}

func StartSubscribers(ctx context.Context, handlers ...StatusHandler) error {
	if _, err := os.Stat(task.ShimSocketPath); errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(task.ShimSocketPath, os.ModePerm); err != nil {
			return err
		}
	}

	socks, err := os.ReadDir(task.ShimSocketPath)
	if err != nil {
		return fmt.Errorf("error listing file in shim socket path: %s", err)
	}

	for _, sock := range socks {
		sock := sock
		go func() {
			if err := subscribe(ctx, filepath.Join(task.ShimSocketPath, sock.Name()), handlers); err != nil {
				slog.Error("error subscribing", "sock", sock.Name(), "err", err)
			}
		}()
	}

	go watchForShims(ctx, handlers)

	return nil
}

func subscribe(ctx context.Context, sock string, handlers []StatusHandler) error {
	log := slog.With("sock", sock)
	log.Info("subscribing to status events")

	conn, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}

	shimClient := v1.NewShimClient(ttrpc.NewClient(conn))
	// not sure why but the emptypb needs to be set in order for the subscribe
	// to be received
	client, err := shimClient.SubscribeStatus(ctx, &v1.SubscribeStatusRequest{Empty: &emptypb.Empty{}})
	if err != nil {
		return err
	}

	for {
		status, err := client.Recv()
		if err != nil {
			if err == io.EOF || errors.Is(err, ttrpc.ErrClosed) {
				log.Info("subscribe closed")
			} else {
				log.Error("subscribe closed", "err", err)
			}
			break
		}
		clog := slog.With("container", status.Name, "pod", status.PodName,
			"namespace", status.PodNamespace, "phase", status.Phase)
		for _, h := range handlers {
			if err := h.Handle(ctx, status); err != nil {
				clog.Error("handling status update", "err", err)
			}
		}
	}

	return nil
}

func watchForShims(ctx context.Context, handlers []StatusHandler) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := watcher.Add(task.ShimSocketPath); err != nil {
		return err
	}

	for {
		select {
		case event := <-watcher.Events:
			switch event.Op {
			case fsnotify.Create:
				if err := subscribe(ctx, event.Name, handlers); err != nil {
					slog.Error("error subscribing", "sock", event.Name, "err", err)
				}
			}
		case err := <-watcher.Errors:
			slog.Error("watch error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}
