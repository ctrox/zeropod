package reuse

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/containerd/cgroups/v3/cgroup2"
	"github.com/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/ctrox/zeropod/activator"
)

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS reuseport reuseport.c -- -I/headers
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS sockopt sockopt.c
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS tracker tracker.c

type network string

const (
	networkTCP4     network = "tcp4"
	networkTCP6     network = "tcp6"
	networkTCP6ONLY network = "tcp"
)

type Activator struct {
	ports          []uint16
	mu             sync.Mutex
	wakeListeners  map[listenerKey]*wakeListener
	probeListeners map[listenerKey]*probeListener
	appListeners   map[listenerKey]*appListener
	wakeInodes     []uint64
	restoreHook    activator.RestoreHook
	log            *log.Entry
	ns             ns.NetNS
	started        atomic.Bool
	sockOptLink    link.Link
	sockoptObjects *sockoptObjects
	trackerLink    link.Link
	trackerObjs    *trackerObjects
	cgroupsPath    string
	sandboxPid     int
}

const (
	appKey   = 0
	wakeKey  = 1
	probeKey = 2
)

func New(ctx context.Context, ns ns.NetNS, cgroupsPath string) (*Activator, error) {
	act := &Activator{
		ns:            ns,
		cgroupsPath:   cgroupsPath,
		log:           log.GetLogger(ctx),
		sandboxPid:    parsePidFromNetNS(ns),
		wakeListeners: make(map[listenerKey]*wakeListener),
		appListeners:  make(map[listenerKey]*appListener),
	}
	if err := act.LoadBPF(); err != nil {
		return nil, fmt.Errorf("loading ebpf: %w", err)
	}
	return act, nil
}

func (act *Activator) LoadBPF() error {
	if err := rlimit.RemoveMemlock(); err != nil {
		return err
	}

	path, err := cgroup2.PidGroupPath(act.sandboxPid)
	if err != nil {
		return err
	}
	// use filepath.Dir to get parent cgroup of the cri-container
	hostCgroupPath := filepath.Join("/sys/fs/cgroup", filepath.Dir(path))
	act.log.Debugf("attaching setsockopt to cgroup %s", hostCgroupPath)

	sockoptObjs := &sockoptObjects{}
	if err := loadSockoptObjects(sockoptObjs, nil); err != nil {
		return fmt.Errorf("loading sockopt objects: %w", err)
	}
	act.sockoptObjects = sockoptObjs
	sockOptLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    hostCgroupPath,
		Attach:  ebpf.AttachCGroupSetsockopt,
		Program: sockoptObjs.Setsockopt,
	})
	if err != nil {
		return err
	}
	act.sockOptLink = sockOptLink

	trackerObjs := &trackerObjects{}
	if err := loadTrackerObjects(trackerObjs, nil); err != nil {
		return fmt.Errorf("loading sockopt objects: %w", err)
	}
	act.trackerObjs = trackerObjs

	trackerLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    hostCgroupPath,
		Attach:  ebpf.AttachCGroupInetIngress,
		Program: trackerObjs.TrackIngress,
	})
	if err != nil {
		return err
	}
	act.trackerLink = trackerLink
	return nil
}

func parsePidFromNetNS(nn ns.NetNS) int {
	parts := strings.Split(nn.Path(), "/")
	if len(parts) < 3 {
		return 0
	}

	pid, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0
	}

	return pid
}

func (act *Activator) Start(ctx context.Context, connHook activator.ConnHook, restoreHook activator.RestoreHook, pid int, ports ...uint16) error {
	act.ports = ports
	act.restoreHook = restoreHook

	before := time.Now()
	if err := act.registerListeners(pid); err != nil {
		act.log.WithError(err).Error("registering listeners")
		return err
	}
	act.log.Infof("registered listeners in %s", time.Since(before))

	act.started.Store(true)
	return act.initActivityTracker()
}

func (act *Activator) Started() bool {
	return act.started.Load()
}

func (act *Activator) Stop() error {
	act.mu.Lock()
	defer act.mu.Unlock()
	for _, wl := range act.wakeListeners {
		wl.close()
	}
	if act.sockoptObjects != nil {
		act.sockoptObjects.Close()
	}
	if act.sockOptLink != nil {
		act.sockOptLink.Close()
	}
	if act.trackerObjs != nil {
		act.trackerObjs.Close()
	}
	if act.trackerLink != nil {
		act.trackerLink.Close()
	}
	return nil
}

func (act *Activator) LastActivity(port uint16) (time.Time, error) {
	if !act.Started() {
		return time.Time{}, nil
	}

	// the old activator used uint16 so we just convert it first
	puint32 := uint32(port)
	var val uint64
	if err := act.trackerObjs.SocketTracker.Lookup(&puint32, &val); err != nil {
		return time.Time{}, fmt.Errorf("looking up %d: %w", port, err)
	}

	if val == 0 {
		return time.Time{}, activator.NoActivityRecordedErr{}
	}

	return activator.ConvertBPFTime(val)
}

func (act *Activator) initActivityTracker() error {
	for _, port := range act.ports {
		val := uint64(0)
		puint32 := uint32(port)
		if err := act.trackerObjs.SocketTracker.Put(&puint32, &val); err != nil {
			return fmt.Errorf("unable to init activity tracker for port %d: %w", port, err)
		}
	}
	return nil
}

func (act *Activator) wake(network network) error {
	if act.restoreHook != nil {
		pid, err := act.restoreHook()
		if err != nil {
			act.log.WithError(err).Error("restore hook")
			return err
		}
		before := time.Now()
		if err := act.registerListeners(pid); err != nil {
			act.log.WithError(err).Error("registering listeners")
			return err
		}
		act.log.Infof("registered listeners in %s", time.Since(before))
	}
	act.mu.Lock()
	defer act.mu.Unlock()
	for _, wl := range act.wakeListeners {
		wl.closeListener()
	}
	// sk_reuseport/migrate only seems to migrate pending connections to the
	// wake listener only when a new conn comes in. We call poke which just
	// dials and immediately closes to trigger the migration.
	for _, port := range act.ports {
		if err := act.poke(port, network); err != nil {
			act.log.WithError(err).Error("poke app listener")
		}
	}
	return nil
}

func (act *Activator) poke(port uint16, network network) error {
	return act.ns.Do(func(nn ns.NetNS) error {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if network == networkTCP6 || network == networkTCP6ONLY {
			addr = fmt.Sprintf("[::1]:%d", port)
		}
		dialer := net.Dialer{Timeout: time.Second}
		c, err := dialer.Dial(string(network), addr)
		if err == nil {
			return c.Close()
		}
		return err
	})
}

func (act *Activator) ScaleDown() error {
	act.mu.Lock()
	defer act.mu.Unlock()
	for _, wl := range act.wakeListeners {
		wl.closeListener()
	}
	act.wakeInodes = []uint64{}
	for k := range act.wakeListeners {
		act.log.Infof("spawning wake listener: %v", k)
		if err := act.listenWake(k.port, k.network, act.wakeListeners[k]); err != nil {
			return err
		}
	}
	act.log.Info("listening for new connections on wake listener")
	act.log.Infof("wakeListeners: %d: %v", len(act.wakeListeners), act.wakeListeners)
	return nil
}
