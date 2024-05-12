package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	v1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/zeropod"
	"github.com/prometheus/client_golang/prometheus"
)

const ShimSocketPath = "/run/zeropod/s/"

func shimSocketAddress(id string) string {
	return fmt.Sprintf("unix://%s.sock", filepath.Join(ShimSocketPath, id))
}

func startShimServer(ctx context.Context, id string) {
	socket := shimSocketAddress(id)
	listener, err := shim.NewSocket(socket)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			log.G(ctx).WithError(err)
			return
		}

		if shim.CanConnect(socket) {
			log.G(ctx).Debug("shim socket already exists, skipping server start")
			return
		}

		if err := shim.RemoveSocket(socket); err != nil {
			log.G(ctx).WithError(fmt.Errorf("remove pre-existing socket: %w", err))
		}

		listener, err = shim.NewSocket(socket)
		if err != nil {
			log.G(ctx).WithError(err).Error("failed to create shim listener")
		}
	}

	log.G(ctx).Infof("starting shim server at %s", socket)
	// write shim address to filesystem
	if err := shim.WriteAddress("shim_address", socket); err != nil {
		log.G(ctx).WithError(err).Errorf("failed to write shim address")
		return
	}

	s, err := ttrpc.NewServer()
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to create ttrpc server")
		return
	}
	defer s.Close()
	v1.RegisterShimService(s, &shimService{metrics: zeropod.NewRegistry()})

	defer func() {
		listener.Close()
		os.Remove(socket)
	}()
	go s.Serve(ctx, listener)

	<-ctx.Done()

	log.G(ctx).Info("stopping metrics server")
	listener.Close()
	s.Close()
	_ = os.RemoveAll(socket)
}

// shimService is an extension to the shim task service to provide
// zeropod-specific functions like metrics.
type shimService struct {
	metrics *prometheus.Registry
}

// Metrics implements v1.ShimService.
func (s *shimService) Metrics(context.Context, *v1.MetricsRequest) (*v1.MetricsResponse, error) {
	mfs, err := s.metrics.Gather()
	return &v1.MetricsResponse{Metrics: mfs}, err
}
