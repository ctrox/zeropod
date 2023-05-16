package zeropod

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/process"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/runc"
)

func (s *Container) scaleDown(ctx context.Context, container *runc.Container, p process.Process) error {
	if s.cfg.Stateful {
		// not sure what is causing this but without adding these iptables
		// rules here already, connections during scaling down sometimes
		// timeout, even though criu should add these rules before
		// checkpointing.
		if err := addCriuIPTablesRules(s.netNS); err != nil {
			return err
		}

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
	if err := s.startActivator(ctx, container); err != nil {
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
	if err := removeCriuIPTablesRules(s.netNS); err != nil {
		log.G(ctx).Errorf("unable to restore iptables: %s", err)
		return err
	}

	log.G(ctx).Infof("activator started and net-lock removed in %s", time.Since(beforeActivator))

	return nil
}

func (s *Container) checkpoint(ctx context.Context, container *runc.Container, p process.Process) error {
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

	return nil
}

func addCriuIPTablesRules(netNS ns.NetNS) error {
	const SOCCR_MARK = "0xC114"
	const conf = "*filter\n" +
		":CRIU - [0:0]\n" +
		"-I INPUT -j CRIU\n" +
		"-I OUTPUT -j CRIU\n" +
		"-A CRIU -m mark --mark " + SOCCR_MARK + " -j ACCEPT\n" +
		"-A CRIU -j DROP\n" +
		"COMMIT\n"

	return ipTablesRestore(conf, netNS)
}

func removeCriuIPTablesRules(netNS ns.NetNS) error {
	const conf = "*filter\n" +
		":INPUT ACCEPT [0:0]\n" +
		":FORWARD ACCEPT [0:0]\n" +
		":OUTPUT ACCEPT [0:0]\n" +
		"COMMIT\n"

	return ipTablesRestore(conf, netNS)
}

func ipTablesRestore(conf string, netNS ns.NetNS) error {
	cmd := exec.Command("iptables-restore")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	errors := make(chan error)
	go func() {
		defer stdin.Close()
		_, err := io.WriteString(stdin, conf)
		if err != nil {
			errors <- err
		}
		close(errors)
	}()

	if err := netNS.Do(func(nn ns.NetNS) error {
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("error running iptables-restore: %s: %w", out, err)
		}
		return nil
	}); err != nil {
		return err
	}

	return <-errors
}
