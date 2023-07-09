package zeropod

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/containerd/containerd/integration/remote/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimev1 "k8s.io/cri-api/pkg/apis/runtime/v1"
	runtimev1alpha2 "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// getLogPath gets the log path of the container by connecting back to
// containerd. There might be a less convoluted way to do this.
func getLogPath(ctx context.Context, containerID string) (string, error) {
	endpoint := os.Getenv("GRPC_ADDRESS")
	if len(endpoint) == 0 {
		endpoint = strings.TrimSuffix(os.Getenv("TTRPC_ADDRESS"), ".ttrpc")
	}

	cri, err := newRuntimeService(endpoint, time.Second)
	if err != nil {
		return "", err
	}

	return cri.LogPath(ctx, containerID)
}

type criService interface {
	LogPath(ctx context.Context, containerID string) (string, error)
}

// in order to support older containerd versions this CRI service implements
// returning the log path of a container for CRI v1alpha2 and v1.
type compatCRIService struct {
	v1alpha2 runtimev1alpha2.RuntimeServiceClient
	v1       runtimev1.RuntimeServiceClient
}

func (c compatCRIService) LogPath(ctx context.Context, containerID string) (string, error) {
	resp, err := c.v1.ContainerStatus(ctx, &runtimev1.ContainerStatusRequest{ContainerId: containerID})
	if err != nil {
		// if v1 fails we fall back to v1alpha2
		r, err := c.v1alpha2.ContainerStatus(ctx, &runtimev1alpha2.ContainerStatusRequest{ContainerId: containerID})
		if err != nil {
			return "", err
		}
		return r.Status.LogPath, nil
	}

	return resp.Status.LogPath, err
}

// newRuntimeService creates a new internalapi.RuntimeService.
func newRuntimeService(endpoint string, connectionTimeout time.Duration) (criService, error) {
	addr, dialer, err := util.GetAddressAndDialer(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()

	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialer))
	if err != nil {
		return nil, err
	}

	return &compatCRIService{
		v1alpha2: runtimev1alpha2.NewRuntimeServiceClient(conn),
		v1:       runtimev1.NewRuntimeServiceClient(conn),
	}, nil
}
