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
)

// getLogPath gets the log path of the container by connecting back to
// containerd. There might be a less convoluted way to do this.
func getLogPath(ctx context.Context, containerID string) (string, error) {
	endpoint := os.Getenv("GRPC_ADDRESS")
	if len(endpoint) == 0 {
		endpoint = strings.TrimSuffix(os.Getenv("TTRPC_ADDRESS"), ".ttrpc")
	}

	addr, dialer, err := util.GetAddressAndDialer(endpoint)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dialer))
	if err != nil {
		return "", err
	}

	resp, err := runtimev1.NewRuntimeServiceClient(conn).ContainerStatus(ctx, &runtimev1.ContainerStatusRequest{ContainerId: containerID})
	if err != nil {
		return "", err
	}

	return resp.Status.LogPath, nil
}
