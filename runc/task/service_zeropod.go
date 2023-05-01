package task

import (
	"context"
	"os"

	"github.com/ctrox/zeropod/activator"
	"github.com/ctrox/zeropod/runc"
	"github.com/sirupsen/logrus"

	cgroupsv2 "github.com/containerd/cgroups/v2"
	"github.com/containerd/cgroups/v3/cgroup1"
	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/userns"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/vishvananda/netns"
)

// StartZeropod starts a zeropod process
func (s *service) StartZeropod(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	// switch to network ns of container and start our activator listener
	netNSPath, err := runc.GetNetworkNS(ctx, container.Bundle)
	if err != nil {
		return nil, err
	}

	ns, err := netns.GetFromPath(netNSPath)
	if err != nil {
		return nil, err
	}

	if err := netns.Set(ns); err != nil {
		return nil, err
	}

	s.activator.Do(func() {
		// create a new context in order to not run into deadline of parent context
		ctx = log.WithLogger(context.Background(), log.G(ctx).WithField("runtime", runc.RuntimeName))
		log.G(ctx).Printf("starting activator")
		// TODO: extract this port from container
		port := 5678
		srv := activator.NewServer(ctx, port)

		s.shutdown.RegisterCallback(func(ctx context.Context) error {
			// stop server on shutdown
			srv.Stop()
			return nil
		})

		if err := srv.Start(ctx, func(f *os.File) error {
			log.G(ctx).Printf("got a request")

			// hold the send lock so that the start events are sent before any exit events in the error case
			s.eventSendMu.Lock()
			p, err := container.Run(ctx, r, f)
			if err != nil {
				s.eventSendMu.Unlock()
				return errdefs.ToGRPC(err)
			}

			switch r.ExecID {
			case "":
				switch cg := container.Cgroup().(type) {
				case cgroup1.Cgroup:
					if err := s.ep.Add(container.ID, cg); err != nil {
						logrus.WithError(err).Error("add cg to OOM monitor")
					}
				case *cgroupsv2.Manager:
					allControllers, err := cg.RootControllers()
					if err != nil {
						logrus.WithError(err).Error("failed to get root controllers")
					} else {
						if err := cg.ToggleControllers(allControllers, cgroupsv2.Enable); err != nil {
							if userns.RunningInUserNS() {
								logrus.WithError(err).Debugf("failed to enable controllers (%v)", allControllers)
							} else {
								logrus.WithError(err).Errorf("failed to enable controllers (%v)", allControllers)
							}
						}
					}
					if err := s.ep.Add(container.ID, cg); err != nil {
						logrus.WithError(err).Error("add cg to OOM monitor")
					}
				}

				s.send(&eventstypes.TaskStart{
					ContainerID: container.ID,
					Pid:         uint32(p.Pid()),
				})
			default:
				s.send(&eventstypes.TaskExecStarted{
					ContainerID: container.ID,
					ExecID:      r.ExecID,
					Pid:         uint32(p.Pid()),
				})
			}
			s.eventSendMu.Unlock()
			return nil
		}); err != nil {
			log.G(ctx).Fatalf("failed to start server on port %d: %s", port, err)
		}
	})

	s.eventSendMu.Lock()
	s.send(&eventstypes.TaskStart{
		ContainerID: container.ID,
		Pid:         uint32(1337),
	})
	s.eventSendMu.Unlock()

	return &taskAPI.StartResponse{
		Pid: uint32(1337),
	}, nil
}
