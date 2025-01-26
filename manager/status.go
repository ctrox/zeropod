// Package manager contains most of the implementation of the zeropod-manager
// node daemon. It takes care of loading eBPF programs, providing metrics and
// monitors the shims for status updates.
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
	"time"

	"github.com/containerd/ttrpc"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/runc/task"
	"github.com/fsnotify/fsnotify"
	"google.golang.org/protobuf/types/known/emptypb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var connectBackoff = wait.Backoff{
	Steps:    5,
	Duration: 100 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

type PodHandler interface {
	// Handle a status update with the associated pod. Changes made to
	// the pod object will be applied after all handlers have been called.
	// Pod status updates are ignored and won't be applied.
	Handle(context.Context, *v1.ContainerStatus, *corev1.Pod) error
}

type PodHandlerWithClient interface {
	PodHandler
	InjectClient(v1.ShimClient)
}

type subscriber struct {
	log             *slog.Logger
	kube            client.Client
	subscribeClient v1.Shim_SubscribeStatusClient
	podHandlers     []PodHandler
}

func StartSubscribers(ctx context.Context, log *slog.Logger, kube client.Client, podHandlers ...PodHandler) error {
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
			if err := subscribe(ctx, log, filepath.Join(task.ShimSocketPath, sock.Name()), kube, podHandlers); err != nil {
				log.Error("error subscribing", "sock", sock.Name(), "err", err)
			}
		}()
	}

	go watchForShims(ctx, log, kube, podHandlers)

	return nil
}

func subscribe(ctx context.Context, log *slog.Logger, sock string, kube client.Client, handlers []PodHandler) error {
	log.With("sock", sock).Info("subscribing to status events")
	shimClient, err := newShimClient(ctx, sock)
	if err != nil {
		return err
	}
	// not sure why but the emptypb needs to be set in order for the subscribe to be received
	subscribeClient, err := shimClient.SubscribeStatus(ctx, &v1.SubscribeStatusRequest{Empty: &emptypb.Empty{}})
	if err != nil {
		return err
	}

	for _, handler := range handlers {
		if ph, ok := handler.(PodHandlerWithClient); ok {
			ph.InjectClient(shimClient)
		}
	}

	s := subscriber{
		log:             log.With("sock", sock),
		kube:            kube,
		subscribeClient: subscribeClient,
		podHandlers:     handlers,
	}
	return s.receive(ctx)
}

func newShimClient(ctx context.Context, sock string) (v1.ShimClient, error) {
	var conn net.Conn
	// the socket file might exist but it can take bit until the server is
	// listening. We retry with a backoff.
	if err := retry.OnError(
		connectBackoff,
		func(err error) bool {
			// always retry
			return true
		},
		func() error {
			var d net.Dialer
			c, err := d.DialContext(ctx, "unix", sock)
			if err != nil {
				return err
			}
			conn = c
			return nil
		},
	); err != nil {
		return nil, err
	}

	return v1.NewShimClient(ttrpc.NewClient(conn)), nil
}

func (s *subscriber) receive(ctx context.Context) error {
	for {
		status, err := s.subscribeClient.Recv()
		if err != nil {
			if err == io.EOF || errors.Is(err, ttrpc.ErrClosed) {
				s.log.Info("subscribe closed")
			} else {
				s.log.Error("subscribe closed", "err", err)
			}
			break
		}
		clog := s.log.With("container", status.Name, "pod", status.PodName,
			"namespace", status.PodNamespace, "phase", status.Phase)
		if err := s.onStatus(ctx, status); err != nil {
			clog.Error("handling status update", "err", err)
		}
	}

	return nil
}

func (s *subscriber) onStatus(ctx context.Context, status *v1.ContainerStatus) error {
	if len(s.podHandlers) > 0 {
		if err := s.handlePod(ctx, status); err != nil {
			return err
		}
	}
	return nil
}

func (s *subscriber) handlePod(ctx context.Context, status *v1.ContainerStatus) error {
	pod := &corev1.Pod{}
	podName := types.NamespacedName{Name: status.PodName, Namespace: status.PodNamespace}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := s.kube.Get(ctx, podName, pod); err != nil {
			return fmt.Errorf("getting pod: %w", err)
		}
		// as a pod handler might want to resize the pod resources, we store all
		// containers before calling the handlers
		containersBefore := pod.DeepCopy().Spec.Containers
		for _, p := range s.podHandlers {
			if err := p.Handle(ctx, status, pod); err != nil {
				return err
			}
		}
		// store containers after and revert the pod spec so we can call the
		// update for all other fields
		containersAfter := pod.DeepCopy().Spec.Containers
		pod.Spec.Containers = containersBefore

		if err := s.kube.Update(ctx, pod); err != nil {
			return fmt.Errorf("updating pod: %w", err)
		}
		// now that other field updates succeeded, we apply the containers again
		// and try to resize
		pod.Spec.Containers = containersAfter
		if err := s.kube.SubResource("resize").Update(ctx, pod); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			s.log.Info("updating pod resources using resize failed, falling back to update")
			if err := s.kube.Update(ctx, pod); err != nil {
				return fmt.Errorf("updating pod: %w", err)
			}
		}
		return nil
	}); err != nil {
		if apierrors.IsInvalid(err) {
			s.log.Error("in-place scaling failed, ensure InPlacePodVerticalScaling feature flag is enabled")
		}
		return err
	}
	return nil
}

func watchForShims(ctx context.Context, log *slog.Logger, kube client.Client, podHandlers []PodHandler) error {
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
				go func() {
					if err := subscribe(ctx, log, event.Name, kube, podHandlers); err != nil {
						log.Error("error subscribing", "sock", event.Name, "err", err)
					}
				}()
			}
		case err := <-watcher.Errors:
			log.Error("watch error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}
