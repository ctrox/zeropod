package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/activator"
	"github.com/ctrox/zeropod/socket"
	"github.com/fsnotify/fsnotify"
)

type Redirector struct {
	sync.Mutex
	activators map[int]*activator.BPF
	log        *slog.Logger
}

// AttachRedirectors scans the zeropod maps path in the bpf file system for
// directories named after the pid of the sandbox container. It does an
// initial iteration over all directories and then starts a goroutine which
// watches for fsevents. When the associated netns of the sandbox container
// can be found it attaches the redirector BPF programs to the network
// interfaces of the sandbox. The directories are expected to be created by
// the zeropod shim on startup.
func AttachRedirectors(ctx context.Context, log *slog.Logger) error {
	r := &Redirector{
		activators: make(map[int]*activator.BPF),
		log:        log,
	}

	if _, err := os.Stat(activator.MapsPath()); os.IsNotExist(err) {
		r.log.Info("maps path not found, creating", "path", activator.MapsPath())
		if err := os.Mkdir(activator.MapsPath(), os.ModePerm); err != nil {
			return err
		}
	}

	pids, err := r.getSandboxPids()
	if err != nil {
		return err
	}

	if len(pids) == 0 {
		r.log.Info("no sandbox pids found")
	}

	for _, pid := range pids {
		if err := statNetNS(pid); os.IsNotExist(err) {
			r.log.Info("net ns not found, removing leftover pid", "path", netNSPath(pid))
			os.RemoveAll(activator.PinPath(pid))
			continue
		}

		if err := r.attachRedirector(pid); err != nil {
			return err
		}
	}

	go r.watchForSandboxPids(ctx)

	return nil
}

func (r *Redirector) watchForSandboxPids(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	if err := watcher.Add(activator.MapsPath()); err != nil {
		return err
	}

	for {
		select {
		// watch for events
		case event := <-watcher.Events:
			if filepath.Base(event.Name) == socket.TCPEventsMap {
				continue
			}

			pid, err := strconv.Atoi(filepath.Base(event.Name))
			if err != nil {
				r.log.Warn("unable to parse pid from added name", "name", filepath.Base(event.Name))
				continue
			}

			if err := statNetNS(pid); err != nil {
				r.log.Warn("ignoring pid as net ns was not found", "pid", pid)
				continue
			}

			switch event.Op {
			case fsnotify.Create:
				if err := r.attachRedirector(pid); err != nil {
					r.log.Error("unable to attach redirector", "pid", pid, "err", err)
				}
			case fsnotify.Remove:
				r.Lock()
				if act, ok := r.activators[pid]; ok {
					r.log.Info("cleaning up activator", "pid", pid)
					if err := act.Cleanup(); err != nil {
						r.log.Error("error cleaning up redirector", "err", err)
					}
				}
				r.Unlock()
			}
		case err := <-watcher.Errors:
			r.log.Error("watch error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}

func (r *Redirector) attachRedirector(pid int) error {
	bpf, err := activator.InitBPF(pid, r.log)
	if err != nil {
		return fmt.Errorf("unable to initialize BPF: %w", err)
	}
	r.Lock()
	r.activators[pid] = bpf
	r.Unlock()

	netNS, err := ns.GetNS(netNSPath(pid))
	if err != nil {
		return err
	}

	if err := netNS.Do(func(nn ns.NetNS) error {
		//  TODO: is this really always eth0?
		// as for loopback, this is required for port-forwarding to work
		ifaces := []string{"eth0", "lo"}
		r.log.Info("attaching redirector for sandbox", "pid", pid, "links", ifaces)
		return bpf.AttachRedirector(ifaces...)
	}); err != nil {
		return err
	}

	return nil
}

func statNetNS(pid int) error {
	_, err := os.Stat(netNSPath(pid))
	return err
}

func netNSPath(pid int) string {
	return fmt.Sprintf("/hostproc/%d/ns/net", pid)
}

func (r *Redirector) getSandboxPids() ([]int, error) {
	f, err := os.Open(activator.MapsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	dirs, err := f.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	intPids := make([]int, 0, len(dirs))
	for _, dir := range dirs {
		if dir == socket.TCPEventsMap {
			continue
		}

		intPid, err := strconv.Atoi(dir)
		if err != nil {
			r.log.Warn("unable to parse pid from dir name", "name", dir)
			continue
		}

		// before adding this pid, check if the corresponding network ns
		// actually exists. This is important when running in a kind environment
		// where the bpffs is shared between different "nodes".
		if err := statNetNS(intPid); err == nil {
			intPids = append(intPids, intPid)
		}
	}

	return intPids, nil
}
