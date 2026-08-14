package reuse

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
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

type Activator struct {
	*Config
	ports          []uint16
	mu             sync.Mutex
	listeners      map[listenerKey]*listenerGroup
	wakeInodes     []uint64
	log            *log.Entry
	ns             ns.NetNS
	started        atomic.Bool
	sockoptLink    link.Link
	bindV4Link     link.Link
	bindV6Link     link.Link
	sockoptObjects *sockoptObjects
	trackerLink    link.Link
	trackerObjs    *trackerObjects
	cgroupsPath    string
	sandboxPid     int
	register       sync.Mutex
	registeredWake atomic.Bool
}

const (
	appKey   = 0
	wakeKey  = 1
	probeKey = 2
)

func New(ctx context.Context, ns ns.NetNS, cgroupsPath string, opts ...Option) (*Activator, error) {
	cfg := &Config{}
	for _, opt := range opts {
		opt(cfg)
	}
	act := &Activator{
		ns:          ns,
		cgroupsPath: cgroupsPath,
		log:         log.GetLogger(ctx),
		sandboxPid:  parsePidFromNetNS(ns),
		listeners:   make(map[listenerKey]*listenerGroup),
		Config:      cfg,
	}
	if err := act.LoadBPF(); err != nil {
		return nil, fmt.Errorf("loading ebpf: %w", err)
	}
	act.log.Debug("activator created")
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
	sockoptLink, err := link.AttachCgroup(link.CgroupOptions{
		Path:    hostCgroupPath,
		Attach:  ebpf.AttachCGroupSetsockopt,
		Program: sockoptObjs.Setsockopt,
	})
	if err != nil {
		return err
	}
	act.sockoptLink = sockoptLink
	bindV4Link, err := link.AttachCgroup(link.CgroupOptions{
		Path:    hostCgroupPath,
		Attach:  ebpf.AttachCGroupInet4Bind,
		Program: sockoptObjs.BindV4,
	})
	if err != nil {
		return err
	}
	act.bindV4Link = bindV4Link
	bindV6Link, err := link.AttachCgroup(link.CgroupOptions{
		Path:    hostCgroupPath,
		Attach:  ebpf.AttachCGroupInet6Bind,
		Program: sockoptObjs.BindV6,
	})
	if err != nil {
		return err
	}
	act.bindV6Link = bindV6Link

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

type Config struct {
	trackerIgnoreLocalhost bool
	probeAddr              *netip.Addr
	restoreHook            activator.RestoreHook
}

type Option func(cfg *Config)

func TrackerIgnoreLocalhost(ignore bool) Option {
	return func(cfg *Config) {
		cfg.trackerIgnoreLocalhost = ignore
	}
}

func RestoreHook(restoreHook activator.RestoreHook) Option {
	return func(cfg *Config) {
		cfg.restoreHook = restoreHook
	}
}

func ProbeAddr(addr *netip.Addr) Option {
	return func(cfg *Config) {
		cfg.probeAddr = addr
	}
}

func (act *Activator) Start(ctx context.Context, pid int, listeners activator.Listeners, skipStart bool) error {
	act.ports = listeners.Ports()

	if skipStart {
		for _, ln := range listeners {
			net := ln.Network
			if net == "" {
				net = activator.NetworkTCPAny
			}
			key := listenerKey{port: ln.Port, network: net}
			objs := &reuseportObjects{}
			if err := loadReuseportObjects(objs, &ebpf.CollectionOptions{}); err != nil {
				return fmt.Errorf("loading reuseport objects: %w", err)
			}
			if act.probeAddr != nil {
				if err := objs.ProbeAddr.Set(act.probeAddrValue()); err != nil {
					return err
				}
			}
			act.mu.Lock()
			act.listeners[key] = &listenerGroup{reuse: objs}
			act.mu.Unlock()
		}
		// we are done loading bpf
		btf.FlushKernelSpec()
		if err := act.Reset(); err != nil {
			return err
		}
	} else {
		before := time.Now()
		if err := act.registerListeners(pid); err != nil {
			act.log.WithError(err).Error("registering listeners")
			return err
		}
		act.log.Debugf("registered listeners in %s", time.Since(before))
	}

	act.started.Store(true)
	return act.initSocketTracker()
}

func (act *Activator) Started() bool {
	return act.started.Load()
}

func (act *Activator) Stop(_ context.Context) {
	act.mu.Lock()
	defer act.mu.Unlock()
	for _, ln := range act.listeners {
		ln.wake.close()
		ln.probe.close()
		ln.forwarder.close()
		if ln.reuse != nil {
			ln.reuse.Close()
		}
	}
	if act.sockoptObjects != nil {
		act.sockoptObjects.Close()
	}
	if act.sockoptLink != nil {
		act.sockoptLink.Close()
	}
	if act.bindV4Link != nil {
		act.bindV4Link.Close()
	}
	if act.bindV6Link != nil {
		act.bindV6Link.Close()
	}
	if act.trackerObjs != nil {
		act.trackerObjs.Close()
	}
	if act.trackerLink != nil {
		act.trackerLink.Close()
	}
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

func (act *Activator) initSocketTracker() error {
	if err := act.clearIgnoredAddrs(); err != nil {
		return err
	}
	if err := act.clearSocketTracker(); err != nil {
		return err
	}
	if act.trackerIgnoreLocalhost {
		if err := IgnoreAddr(act.trackerObjs.IgnoredAddrs, "127.0.0.0/8"); err != nil {
			return err
		}
		if err := IgnoreAddr(act.trackerObjs.IgnoredAddrs, "::1/128"); err != nil {
			return err
		}
	}
	if act.probeAddr != nil {
		if err := IgnoreAddr(act.trackerObjs.IgnoredAddrs, act.probeAddr.String()); err != nil {
			return err
		}
	}
	for _, port := range act.ports {
		val := uint64(0)
		puint32 := uint32(port)
		if err := act.trackerObjs.SocketTracker.Put(&puint32, &val); err != nil {
			return fmt.Errorf("unable to init activity tracker for port %d: %w", port, err)
		}
	}
	return nil
}

func (act *Activator) Reload(opts ...Option) error {
	cfg := &Config{}
	for _, opt := range opts {
		opt(cfg)
	}
	act.Config = cfg
	if err := act.initSocketTracker(); err != nil {
		return err
	}
	act.mu.Lock()
	defer act.mu.Unlock()
	for _, ln := range act.listeners {
		if err := ln.reuse.ProbeAddr.Set(act.probeAddrValue()); err != nil {
			return err
		}
	}
	return nil
}

func (act *Activator) clearIgnoredAddrs() error {
	var key trackerIpKey
	var val byte
	iter := act.trackerObjs.IgnoredAddrs.Iterate()
	for iter.Next(&key, &val) {
		if err := act.trackerObjs.IgnoredAddrs.Delete(key); err != nil {
			return err
		}
	}
	return iter.Err()
}

func (act *Activator) clearSocketTracker() error {
	var key uint32
	var val uint64
	iter := act.trackerObjs.SocketTracker.Iterate()
	for iter.Next(&key, &val) {
		if err := act.trackerObjs.SocketTracker.Delete(key); err != nil {
			return err
		}
	}
	return iter.Err()
}

func IgnoreAddr(addrMap *ebpf.Map, ip string) error {
	prefix, err := netip.ParsePrefix(ip)
	if err != nil {
		noPrefix, err := netip.ParseAddr(ip)
		if err != nil {
			return err
		}
		prefix, err = noPrefix.Prefix(noPrefix.BitLen())
		if err != nil {
			return err
		}
	}
	key := trackerIpKey{
		Prefixlen: uint32(prefix.Bits()),
	}

	addr := prefix.Addr()
	if addr.Is4() {
		ip4 := addr.As4()
		copy(key.Addr[:4], ip4[:])
	} else {
		ip6 := addr.As16()
		copy(key.Addr[:], ip6[:])
	}

	var value byte = 0
	return addrMap.Put(&key, value)
}

func (act *Activator) wake(network activator.Network) error {
	closeProbe := false
	if act.restoreHook != nil {
		pid, err := act.restoreHook()
		if err != nil {
			act.log.WithError(err).Error("restore hook")
			return err
		}
		// TODO: should retoreHook return NoCapacity?
		if pid != 0 {
			closeProbe = true
			before := time.Now()
			act.register.Lock()
			if !act.registeredWake.Load() {
				if err := act.registerListeners(pid); err != nil {
					act.log.WithError(err).Error("registering listeners")
					act.register.Unlock()
					return err
				}
				act.registeredWake.Store(true)
				act.register.Unlock()
			}
			act.log.Debugf("registered listeners in %s", time.Since(before))
		}
	}
	act.mu.Lock()
	defer act.mu.Unlock()
	for _, ln := range act.listeners {
		ln.wake.closeListener()
		if !closeProbe {
			continue
		}
		ln.probe.closeListener()
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

func (act *Activator) poke(port uint16, network activator.Network) error {
	return act.ns.Do(func(nn ns.NetNS) error {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if network == activator.NetworkTCPAny || network == activator.NetworkTCP6ONLY {
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

func (act *Activator) Reset() error {
	act.mu.Lock()
	defer act.mu.Unlock()
	act.registeredWake.Store(false)
	for _, ln := range act.listeners {
		ln.wake.closeListener()
		ln.probe.closeListener()
	}
	act.wakeInodes = []uint64{}
	for k := range act.listeners {
		if err := act.ns.Do(func(nn ns.NetNS) error {
			if err := act.listenWake(k.port, k.network, act.listeners[k]); err != nil {
				return err
			}
			if err := act.listenProbe(k.port, k.network, act.listeners[k]); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
		if err := act.attachWake(k.network, act.listeners[k]); err != nil {
			return err
		}
		if err := act.attachProbe(act.listeners[k]); err != nil {
			return err
		}
	}
	act.log.Debugf("listening for new connections on %d listeners: %v", len(act.listeners), act.listeners)
	return nil
}

func (act *Activator) GetListeners() []activator.Listener {
	listeners := []activator.Listener{}
	for k, l := range act.listeners {
		listeners = append(listeners, activator.Listener{
			Port:    k.port,
			Network: k.network,
			UID:     l.app.uid,
		})
	}
	return listeners
}
