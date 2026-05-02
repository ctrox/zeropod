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
	"github.com/fsnotify/fsnotify"
)

type Redirector struct {
	sync.Mutex
	sandboxes     map[int]sandbox
	log           *slog.Logger
	activatorOpts []activator.BPFOpts
}

type sandbox struct {
	activator *activator.BPF
}

// AttachRedirectors scans the zeropod maps path in the bpf file system for
// directories named after the pid of the sandbox container. It does an
// initial iteration over all directories and then starts a goroutine which
// watches for fsevents. When the associated netns of the sandbox container
// can be found it attaches the redirector BPF programs to the network
// interfaces of the sandbox. The directories are expected to be created by
// the zeropod shim on startup.
func AttachRedirectors(ctx context.Context, log *slog.Logger, activatorOpts ...activator.BPFOpts) error {
	r := &Redirector{
		sandboxes:     make(map[int]sandbox),
		log:           log,
		activatorOpts: activatorOpts,
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

	errs := []error{}
	for _, pid := range pids {
		if err := statNetNS(pid); os.IsNotExist(err) {
			r.log.Info("net ns not found, removing leftover pid", "path", netNSPath(pid))
			os.RemoveAll(activator.PinPath(pid))
			continue
		}

		if activator.TCXPinned(pid) {
			r.log.Debug("skipping already pinned attach", "pid", pid)
			continue
		}
		errs = append(errs, r.attachRedirector(pid))
	}

	go r.watchForSandboxPids(ctx)

	return errors.Join(errs...)
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
			if ignoredDir(filepath.Base(event.Name)) {
				continue
			}

			pid, err := strconv.Atoi(filepath.Base(event.Name))
			if err != nil {
				r.log.Warn("unable to parse pid from added name", "name", filepath.Base(event.Name))
				continue
			}

			if activator.TCXPinned(pid, activator.DefaultIfaces...) {
				r.log.Debug("skipping already pinned attach", "pid", pid)
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
				if sb, ok := r.sandboxes[pid]; ok {
					r.log.Info("cleaning up redirector", "pid", pid)
					if err := sb.Remove(); err != nil {
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
	bpf, err := activator.InitBPF(pid, r.log, r.activatorOpts...)
	if err != nil {
		return fmt.Errorf("unable to initialize BPF: %w", err)
	}

	netNS, err := ns.GetNS(netNSPath(pid))
	if err != nil {
		return err
	}

	if err := netNS.Do(func(nn ns.NetNS) error {
		r.log.Info("attaching redirector for sandbox", "pid", pid, "links", activator.DefaultIfaces)
		if err := bpf.AttachRedirector(activator.DefaultIfaces...); err != nil {
			return err
		}

		return err
	}); err != nil {
		return errors.Join(err, bpf.Cleanup())
	}

	r.Lock()
	r.sandboxes[pid] = sandbox{activator: bpf}
	r.Unlock()

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
		if ignoredDir(dir) {
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

func ignoredDir(dir string) bool {
	return dir == activator.SocketTrackerMap ||
		dir == activator.PodKubeletAddrsMapv4 ||
		dir == activator.PodKubeletAddrsMapv6
}

func (sb sandbox) Remove() error {
	errs := []error{sb.activator.Cleanup()}
	return errors.Join(errs...)
}
