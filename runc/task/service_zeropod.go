package task

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/ctrox/zeropod/activator"
	"github.com/ctrox/zeropod/runc"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"

	"github.com/containerd/cgroups"
	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/cri/util"
	"github.com/containerd/containerd/pkg/oom"
	oomv1 "github.com/containerd/containerd/pkg/oom/v1"
	oomv2 "github.com/containerd/containerd/pkg/oom/v2"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containerd/containerd/pkg/shutdown"
	"github.com/containerd/containerd/pkg/stdio"
	"github.com/containerd/containerd/runtime/v2/shim"
	taskAPI "github.com/containerd/containerd/runtime/v2/task"
	"github.com/containerd/containerd/sys/reaper"
	runcC "github.com/containerd/go-runc"
	"github.com/containernetworking/plugins/pkg/ns"
)

func NewZeropodService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TaskService, error) {
	var (
		ep  oom.Watcher
		err error
	)
	if cgroups.Mode() == cgroups.Unified {
		ep, err = oomv2.New(publisher)
	} else {
		ep, err = oomv1.New(publisher)
	}
	if err != nil {
		return nil, err
	}
	go ep.Run(ctx)
	s := &service{
		context:    ctx,
		events:     make(chan interface{}, 128),
		ec:         reaper.Default.Subscribe(),
		ep:         ep,
		shutdown:   sd,
		containers: make(map[string]*runc.Container),
	}
	w := &wrapper{service: s}
	go w.processExits()
	runcC.Monitor = reaper.Default
	if err := s.initPlatform(); err != nil {
		return nil, fmt.Errorf("failed to initialized platform behavior: %w", err)
	}
	go s.forward(ctx, publisher)
	sd.RegisterCallback(func(context.Context) error {
		close(s.events)
		return nil
	})

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			return shim.RemoveSocket(address)
		})
	}

	return w, err
}

type wrapper struct {
	*service

	activator       sync.Mutex
	originalProcess process.Process
	scaledDown      bool
	netNSPath       string

	spec *specs.Spec
	cfg  *ZeropodConfig
}

func (s *wrapper) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	resp, err := s.service.Start(ctx, r)
	if err != nil {
		return nil, err
	}

	container, err := s.getContainer(r.ID)
	if err != nil {
		return nil, err
	}

	p, err := container.Process(r.ExecID)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	sandboxContainer, err := runc.IsSandboxContainer(container.Bundle)
	if err != nil {
		return nil, err
	}

	// if we don't have a sandbox container and no exec ID is set we can
	// checkpoint the container, stop it and start our zeropod in place.
	if !sandboxContainer && len(r.ExecID) == 0 {
		// before we scale down, we store the original process so we can reuse
		// some things and also cleanly shutdown when the time comes.
		s.originalProcess = p

		spec, err := runc.GetSpec(container.Bundle)
		if err != nil {
			return nil, err
		}
		s.spec = spec

		cfg, err := NewConfig(spec)
		if err != nil {
			return nil, err
		}
		s.cfg = cfg

		// let the process start first before we checkpoint. TODO: this should be done async.
		time.Sleep(time.Second * 2)
		if err := s.scaleDown(ctx, container, p); err != nil {
			return &taskAPI.StartResponse{
				Pid: uint32(p.Pid()),
			}, nil
		}
	}

	return resp, err
}

// StartZeropod starts a zeropod process
func (s *wrapper) StartZeropod(ctx context.Context, container *runc.Container) error {
	if len(s.netNSPath) == 0 {
		// get network ns of our container and store it for later use
		netNSPath, err := runc.GetNetworkNS(s.spec)
		if err != nil {
			return err
		}
		s.netNSPath = netNSPath
	}

	// create a new context in order to not run into deadline of parent context
	ctx = log.WithLogger(context.Background(), log.G(ctx).WithField("runtime", runc.RuntimeName))

	log.G(ctx).Infof("starting activator with config: %v", s.cfg)

	srv, err := activator.NewServer(ctx, s.cfg.Port, s.netNSPath)
	if err != nil {
		return err
	}

	s.shutdown.RegisterCallback(func(ctx context.Context) error {
		// stop server on shutdown
		srv.Stop(ctx)
		return nil
	})

	if err := srv.Start(ctx, s.restoreHandler(ctx, container), s.checkpointHandler(ctx)); err != nil {
		log.G(ctx).Errorf("failed to start server: %s", err)
		return err
	}

	log.G(ctx).Printf("activator started")
	return nil
}

func (s *wrapper) restoreHandler(ctx context.Context, container *runc.Container) activator.AcceptFunc {
	return func() (*runc.Container, process.Process, error) {
		log.G(ctx).Printf("got a request")
		beforeRestore := time.Now()

		// hold the send lock so that the start events are sent before any exit events in the error case
		s.eventSendMu.Lock()

		p, err := s.restore(ctx, container)
		if err != nil {
			// restore failed, this is currently unrecoverable, so we shutdown
			// our shim and let containerd recreate it.
			log.G(ctx).Fatalf("error restoring container, exiting shim: %s", err)
			os.Exit(1)
		}
		s.scaledDown = false
		log.G(ctx).Printf("restored process: %d in %s", p.Pid(), time.Since(beforeRestore))

		s.send(&eventstypes.TaskStart{
			ContainerID: container.ID,
			Pid:         uint32(p.Pid()),
		})

		s.eventSendMu.Unlock()

		// before returning we set the net ns again as it might have changed
		// in the meantime. (not sure why that happens though)
		return container, p, nil
	}
}

func (s *wrapper) checkpointHandler(ctx context.Context) activator.ClosedFunc {
	return func(container *runc.Container, p process.Process) error {
		time.Sleep(s.cfg.ScaleDownDuration)
		log.G(ctx).Info("scaling down after scale down duration is up")
		return s.scaleDown(ctx, container, p)
	}
}

func (s *wrapper) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	if len(r.ExecID) == 0 && s.scaledDown {
		container, err := s.getContainer(r.ID)
		if err != nil {
			return nil, err
		}
		p, err := container.Process("")
		if err != nil {
			return nil, err
		}
		log.G(ctx).Infof("requested scaled down process %d to be killed", p.Pid())
		s.originalProcess.SetExited(0)
		p.SetExited(0)
	}

	return s.service.Kill(ctx, r)
}

func (s *wrapper) scaleDown(ctx context.Context, container *runc.Container, p process.Process) error {
	if s.cfg.Stateful {
		if err := s.checkpoint(ctx, container, p); err != nil {
			return err
		}
	} else {
		log.G(ctx).Infof("container is not stateful, scaling down by killing")

		s.scaledDown = true
		if err := p.Kill(ctx, 9, false); err != nil {
			return err
		}
	}

	beforeActivator := time.Now()
	if err := s.StartZeropod(ctx, container); err != nil {
		log.G(ctx).Errorf("unable to start zeropod: %s", err)
		return err
	}

	if !s.cfg.Stateful {
		log.G(ctx).Infof("activator started in %s", time.Since(beforeActivator))
		return nil
	}

	// after checkpointing criu locks the network until the process is
	// restored by inserting some iptables rules. As we start our activator
	// instead of restoring the process right away, we remove these iptables
	// rules by switching into the netns of the container and running
	// iptables-restore. https://criu.org/CLI/opt/--network-lock
	targetNS, err := ns.GetNS(s.netNSPath)
	if err != nil {
		return err
	}

	if err := removeCriuIPTablesRules(targetNS); err != nil {
		log.G(ctx).Errorf("unable to restore iptables: %s", err)
		return err
	}

	log.G(ctx).Infof("activator started and net-lock removed in %s", time.Since(beforeActivator))

	return nil
}

func (s *wrapper) checkpoint(ctx context.Context, container *runc.Container, p process.Process) error {
	snapshotDir := snapshotDir(container.Bundle)

	if err := os.RemoveAll(snapshotDir); err != nil {
		return fmt.Errorf("unable to prepare snapshot dir: %w", err)
	}

	workDir := path.Join(snapshotDir, "work")
	log.G(ctx).Infof("checkpointing process %d of container to %s", p.Pid(), snapshotDir)

	s.scaledDown = true
	beforeCheckpoint := time.Now()
	if err := p.(*process.Init).Checkpoint(ctx, &process.CheckpointConfig{
		Path:                     containerDir(container.Bundle),
		WorkDir:                  workDir,
		Exit:                     true,
		AllowOpenTCP:             true,
		AllowExternalUnixSockets: true,
		AllowTerminal:            false,
		FileLocks:                false,
		EmptyNamespaces:          []string{},
	}); err != nil {
		s.scaledDown = false

		log.G(ctx).Errorf("error checkpointing container: %s", err)
		b, err := os.ReadFile(path.Join(workDir, "dump.log"))
		if err != nil {
			log.G(ctx).Errorf("error reading dump.log: %s", err)
		}

		log.G(ctx).Errorf("dump.log: %s", b)

		return err
	}
	log.G(ctx).Infof("checkpointing done in %s", time.Since(beforeCheckpoint))

	s.send(&eventstypes.TaskCheckpointed{
		ContainerID: container.ID,
	})
	return nil
}

func (s *wrapper) restore(ctx context.Context, container *runc.Container) (process.Process, error) {
	// generate a new container ID as sometimes (probably some race condition)
	// we get a "container with given ID already exists" from runc.
	container.ID = util.GenerateID()
	runtime := process.NewRunc("", container.Bundle, "k8s", "", "", false)

	// TODO: we should somehow reuse the original stdio. For now we just
	// create a file for stdout and stderr.
	p := process.New(container.ID, runtime, stdio.Stdio{
		Stdout: "file://" + s.originalProcess.Stdio().Stdout + "-1",
		Stderr: "file://" + s.originalProcess.Stdio().Stderr + "-1",
	})
	p.Bundle = container.Bundle
	p.Platform = s.platform
	p.WorkDir = filepath.Join(container.Bundle, "work")

	if p.CriuWorkPath == "" {
		// if criu work path not set, use container WorkDir
		p.CriuWorkPath = p.WorkDir
	}

	createConfig := &process.CreateConfig{
		ID:     container.ID,
		Bundle: container.Bundle,
	}

	if s.cfg.Stateful {
		log.G(ctx).Infof("container %s is stateful, restoring from checkpoint", container.ID)
		createConfig.Checkpoint = containerDir(container.Bundle)
	} else {
		log.G(ctx).Infof("restoring %s by starting the process again", container.ID)
	}

	if err := p.Create(ctx, createConfig); err != nil {
		return nil, fmt.Errorf("creation failed during restore: %w", err)
	}

	log.G(ctx).Info("restore: process created")

	if err := p.Start(ctx); err != nil {
		return nil, fmt.Errorf("start failed during restore: %w", err)
	}

	container.SetMainProcess(p)

	s.send(&eventstypes.TaskStart{
		ContainerID: container.ID,
		Pid:         uint32(p.Pid()),
	})

	return p, nil
}

func removeCriuIPTablesRules(netNS ns.NetNS) error {
	const restore = "*filter\n" +
		":INPUT ACCEPT [0:0]\n" +
		":FORWARD ACCEPT [0:0]\n" +
		":OUTPUT ACCEPT [0:0]\n" +
		"COMMIT"

	cmd := exec.Command("iptables-restore")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	errors := make(chan error)
	go func() {
		defer stdin.Close()
		_, err := io.WriteString(stdin, restore)
		if err != nil {
			errors <- err
		}
		close(errors)
	}()

	if err := netNS.Do(func(nn ns.NetNS) error {
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error runing iptables-restore: %s: %w", out, err)
		}
		return nil
	}); err != nil {
		close(errors)
		return err
	}

	return <-errors
}

func (s *wrapper) processExits() {
	for e := range s.ec {
		s.checkProcesses(e)
	}
}

func (s *wrapper) checkProcesses(e runcC.Exit) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, container := range s.containers {
		if !container.HasPid(e.Pid) {
			continue
		}

		for _, p := range container.All() {
			if p.Pid() != e.Pid {
				continue
			}

			if ip, ok := p.(*process.Init); ok {
				// Ensure all children are killed
				if runc.ShouldKillAllOnExit(s.context, container.Bundle) {
					if err := ip.KillAll(s.context); err != nil {
						logrus.WithError(err).WithField("id", ip.ID()).
							Error("failed to kill init's children")
					}
				}
			}

			if s.scaledDown {
				log.G(s.context).Infof("not setting exited because process has scaled down: %v", p.Pid())
				continue
			}

			if p.ID() == s.originalProcess.ID() {
				// we also need to set the original process as being exited so we can exit cleanly
				s.originalProcess.SetExited(0)
			}

			p.SetExited(e.Status)
			s.sendL(&eventstypes.TaskExit{
				ContainerID: container.ID,
				ID:          p.ID(),
				Pid:         uint32(e.Pid),
				ExitStatus:  uint32(e.Status),
				ExitedAt:    p.ExitedAt(),
			})
			return
		}
		return
	}
}

func snapshotDir(bundle string) string {
	return path.Join(bundle, "snapshots")
}

func containerDir(bundle string) string {
	return path.Join(snapshotDir(bundle), "container")
}
