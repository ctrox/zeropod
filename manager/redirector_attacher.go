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
	activators map[int]*activator.BPF
}

// AttachRedirectors scans the zeropod maps path in the bpf file system for
// directories named after the pid of the sandbox container. It does an
// initial iteration over all directories and then starts a goroutine which
// watches for fsevents. When the associated netns of the sandbox container
// can be found it attaches the redirector BPF programs to the network
// interfaces of the sandbox. The directories are expected to be created by
// the zeropod shim on startup.
func AttachRedirectors(ctx context.Context) error {
	r := &Redirector{
		activators: make(map[int]*activator.BPF),
	}

	if _, err := os.Stat(activator.MapsPath()); os.IsNotExist(err) {
		slog.Info("maps path not found, creating", "path", activator.MapsPath())
		if err := os.Mkdir(activator.MapsPath(), os.ModePerm); err != nil {
			return err
		}
	}

	pids, err := getSandboxPids()
	if err != nil {
		return err
	}

	if len(pids) == 0 {
		slog.Info("no sandbox pids found")
	}

	for _, pid := range pids {
		if _, err := os.Stat(netNSPath(pid)); os.IsNotExist(err) {
			slog.Info("net ns not found, removing leftover pid", "path", netNSPath(pid))
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
			pid, err := strconv.Atoi(filepath.Base(event.Name))
			if err != nil {
				slog.Error("unable to parse pid from added name", "name", filepath.Base(event.Name))
				break
			}

			switch event.Op {
			case fsnotify.Create:
				if err := r.attachRedirector(pid); err != nil {
					slog.Error("unable to attach redirector", "pid", pid)
				}
			case fsnotify.Remove:
				r.Lock()
				if act, ok := r.activators[pid]; ok {
					slog.Info("cleaning up activator", "pid", pid)
					if err := act.Cleanup(); err != nil {
						slog.Error("error cleaning up redirector", "err", err)
					}
				}
				r.Unlock()
			}
		case err := <-watcher.Errors:
			slog.Error("watch error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}

func (r *Redirector) attachRedirector(pid int) error {
	bpf, err := activator.InitBPF(pid)
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
		slog.Info("attaching redirector for sandbox", "pid", pid, "links", ifaces)
		return bpf.AttachRedirector(ifaces...)
	}); err != nil {
		return err
	}

	return nil
}

func netNSPath(pid int) string {
	return fmt.Sprintf("/hostproc/%d/ns/net", pid)
}

func getSandboxPids() ([]int, error) {
	f, err := os.Open(activator.MapsPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	pids, err := f.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	intPids := make([]int, 0, len(pids))
	for _, pid := range pids {
		intPid, err := strconv.Atoi(pid)
		if err != nil {
			slog.Error("unable to parse pid from dir name", "name", pid)
			continue
		}
		intPids = append(intPids, intPid)
	}

	return intPids, nil
}
