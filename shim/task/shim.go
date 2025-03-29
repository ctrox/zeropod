package task

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	zshim "github.com/ctrox/zeropod/shim"
	"github.com/prometheus/client_golang/prometheus"
)

const ShimSocketPath = "/run/zeropod/s/"

func shimSocketAddress(containerdSocket string) string {
	return fmt.Sprintf("unix://%s.sock", filepath.Join(ShimSocketPath, path.Base(containerdSocket)))
}

func shimID() (string, error) {
	address, err := shim.ReadAddress("address")
	if err == nil {
		return address, nil
	}

	path, err := filepath.Abs("bootstrap.json")
	if err != nil {
		return "", fmt.Errorf("reading bootstrap.json: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading bootstrap.json: %w", err)
	}
	var params shim.BootstrapParams
	if err := json.Unmarshal(data, &params); err != nil {
		return "", fmt.Errorf("unmarshalling bootstrap.json: %w", err)
	}

	return filepath.Base(params.Address), nil
}

func startShimServer(ctx context.Context, id string, events chan *v1.ContainerStatus) {
	socket := shimSocketAddress(id)
	listener, err := shim.NewSocket(socket)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			log.G(ctx).WithError(err).Error("listening to socket")
			return
		}

		if shim.CanConnect(socket) {
			log.G(ctx).Debug("shim socket already exists, skipping server start")
			return
		}

		if err := shim.RemoveSocket(socket); err != nil {
			log.G(ctx).WithError(err).Error("remove pre-existing socket")
		}

		listener, err = shim.NewSocket(socket)
		if err != nil {
			log.G(ctx).WithError(err).Error("failed to create shim listener")
		}
	}

	log.G(ctx).Infof("starting shim server at %s", socket)
	// write shim address to filesystem
	if err := v1.WriteAddress("shim_address", socket); err != nil {
		log.G(ctx).WithError(err).Errorf("failed to write shim address")
		return
	}

	s, err := ttrpc.NewServer()
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to create ttrpc server")
		return
	}
	defer s.Close()

	v1.RegisterShimService(s, &shimService{metrics: zshim.NewRegistry(), events: events})

	defer func() {
		s.Close()
		listener.Close()
		os.Remove(socket)
	}()
	go s.Serve(ctx, listener)

	<-ctx.Done()

	log.G(ctx).Info("stopping shim server")
}

// shimService is an extension to the shim task service to provide
// zeropod-specific functions like metrics.
type shimService struct {
	metrics *prometheus.Registry
	task    wrapper
	events  chan *v1.ContainerStatus
}

// SubscribeStatus watches for shim events.
func (s *shimService) SubscribeStatus(ctx context.Context, _ *v1.SubscribeStatusRequest, srv v1.Shim_SubscribeStatusServer) error {
	for {
		select {
		case msg := <-s.events:
			if err := srv.Send(msg); err != nil {
				log.G(ctx).Errorf("unable to send event message: %s", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// GetStatus returns the status of a zeropod container.
func (s *shimService) GetStatus(ctx context.Context, req *v1.ContainerRequest) (*v1.ContainerStatus, error) {
	container, ok := s.task.zeropodContainers[req.Id]
	if !ok {
		return nil, fmt.Errorf("could not find zeropod container with id: %s", req.Id)
	}

	return container.Status(), nil
}

// Metrics returns metrics of the zeropod shim instance.
func (s *shimService) Metrics(context.Context, *v1.MetricsRequest) (*v1.MetricsResponse, error) {
	mfs, err := s.metrics.Gather()
	return &v1.MetricsResponse{Metrics: mfs}, err
}
