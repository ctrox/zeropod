package shim

import (
	"context"
	"fmt"
	"net"

	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
)

func (c *Container) restoreCapacityRequest(ctx context.Context) (*nodev1.RestoreCapacityResponse, error) {
	conn, err := net.Dial("unix", nodev1.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("%w: dialing node service: %w", ErrRestoreDial, err)
	}
	defer conn.Close()
	log.G(ctx).Infof("creating restore capacity request for container: %s", c.cfg.ContainerName)

	restoreCapReq := &nodev1.RestoreCapacityRequest{
		PodInfo: &nodev1.PodInfo{
			Name:          c.cfg.PodName,
			Namespace:     c.cfg.PodNamespace,
			ContainerName: c.cfg.ContainerName,
		},
	}
	nodeClient := nodev1.NewNodeClient(ttrpc.NewClient(conn))
	resp, err := nodeClient.RestoreCapacity(ctx, restoreCapReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRestoreRequestFailed, err)
	}
	return resp, nil
}
