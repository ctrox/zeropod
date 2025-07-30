package socket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/ctrox/zeropod/activator"
	"golang.org/x/sys/unix"
)

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -target amd64,arm64 -cflags $BPF_CFLAGS bpf kprobe.c -- -I/headers

const (
	TCPEventsMap         = "tcp_events"
	PodKubeletAddrsMapv4 = "pod_kubelet_addrs_v4"
	PodKubeletAddrsMapv6 = "pod_kubelet_addrs_v6"
)

var mapNames = []string{
	TCPEventsMap,
	PodKubeletAddrsMapv4,
	PodKubeletAddrsMapv6,
}

// LoadEBPFTracker loads the eBPF program and attaches the kretprobe to track
// connections system-wide.
func LoadEBPFTracker() (Tracker, func() error, error) {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, nil, err
	}

	pinPath := activator.MapsPath()
	if err := os.MkdirAll(pinPath, os.ModePerm); err != nil {
		return nil, nil, fmt.Errorf("failed to create bpf fs subpath: %w", err)
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	collectionOpts := &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			// Pin the map to the BPF filesystem and configure the
			// library to automatically re-write it in the BPF
			// program so it can be re-used if it already exists or
			// create it if not.
			PinPath: pinPath,
		},
	}
	if err := loadBpfObjects(&objs, collectionOpts); err != nil {
		if !errors.Is(err, ebpf.ErrMapIncompatible) {
			return nil, nil, fmt.Errorf("loading objects: %w", err)
		}
		// try to unpin the maps and load again
		for _, mapName := range mapNames {
			if err := os.Remove(filepath.Join(pinPath, mapName)); err != nil && !os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("removing map after incompatibility: %w", err)
			}
		}
		if err := loadBpfObjects(&objs, collectionOpts); err != nil {
			return nil, nil, fmt.Errorf("loading objects: %w", err)
		}
	}

	// in the past we used inet_sock_set_state here but we now use a
	// kretprobe with inet_csk_accept as inet_sock_set_state is not giving us
	// reliable PIDs. https://github.com/iovisor/bcc/issues/2304
	tracker, err := link.Kretprobe("inet_csk_accept", objs.KretprobeInetCskAccept, &link.KprobeOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("linking kprobe: %w", err)
	}
	kubeletDetector, err := link.Kprobe("tcp_rcv_state_process", objs.TcpRcvStateProcess, &link.KprobeOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("linking tcp rcv kprobe: %w", err)
	}

	t, err := NewEBPFTracker()
	return t, func() error {
		errs := []error{}
		if err := objs.Close(); err != nil {
			errs = append(errs, err)
		}
		if err := kubeletDetector.Close(); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(append(errs, tracker.Close())...)
	}, err
}

// NewEBPFTracker returns a TCP connection tracker that will keep track of the
// last TCP accept of specific processes. It writes the results to an ebpf map
// keyed with the PID and the value contains the timestamp of the last
// observed accept.
func NewEBPFTracker() (Tracker, error) {
	var resolver PIDResolver
	resolver = noopResolver{}
	// if hostProcPath exists, we're probably running in a test container. We
	// will use the hostResolver instead of using the actual pids.
	if _, err := os.Stat(hostProcPath); err == nil {
		resolver = hostResolver{}
	}

	podKubeletAddrsv4, err := ebpf.LoadPinnedMap(filepath.Join(activator.MapsPath(), PodKubeletAddrsMapv4), &ebpf.LoadPinOptions{})
	if err != nil {
		return nil, err
	}
	podKubeletAddrsv6, err := ebpf.LoadPinnedMap(filepath.Join(activator.MapsPath(), PodKubeletAddrsMapv6), &ebpf.LoadPinOptions{})
	if err != nil {
		return nil, err
	}
	tcpEvents, err := ebpf.LoadPinnedMap(filepath.Join(activator.MapsPath(), TCPEventsMap), &ebpf.LoadPinOptions{})
	return &EBPFTracker{
		PIDResolver:       resolver,
		tcpEvents:         tcpEvents,
		podKubeletAddrsv4: podKubeletAddrsv4,
		podKubeletAddrsv6: podKubeletAddrsv6,
	}, err
}

// PIDResolver allows to customize how the PIDs of the connection tracker are
// resolved. This can be useful if the shim is already running in a container
// (e.g. when using Kind), so it can resolve the PID of the container to the
// ones of the host that ebpf sees.
type PIDResolver interface {
	Resolve(pid uint32) uint32
}

// noopResolver does not resolve anything and just returns the actual pid.
type noopResolver struct{}

func (p noopResolver) Resolve(pid uint32) uint32 {
	return pid
}

type NoActivityRecordedErr struct{}

func (err NoActivityRecordedErr) Error() string {
	return "no activity recorded"
}

type EBPFTracker struct {
	PIDResolver
	tcpEvents         *ebpf.Map
	podKubeletAddrsv4 *ebpf.Map
	podKubeletAddrsv6 *ebpf.Map
}

// TrackPid puts the pid into the TcpEvents map meaning tcp events of the
// process belonging to that pid will be tracked.
func (c *EBPFTracker) TrackPid(pid uint32) error {
	val := uint64(0)
	pid = c.PIDResolver.Resolve(pid)
	if err := c.tcpEvents.Put(&pid, &val); err != nil {
		return fmt.Errorf("unable to put pid %d into bpf map: %w", pid, err)
	}

	return nil
}

// RemovePid removes the pid from the TcpEvents map.
func (c *EBPFTracker) RemovePid(pid uint32) error {
	pid = c.PIDResolver.Resolve(pid)
	return c.tcpEvents.Delete(&pid)
}

// LastActivity returns a time.Time of the last tcp activity recorded of the
// process belonging to the pid (or a child-process of the pid).
func (c *EBPFTracker) LastActivity(pid uint32) (time.Time, error) {
	var val uint64

	pid = c.PIDResolver.Resolve(pid)
	if err := c.tcpEvents.Lookup(&pid, &val); err != nil {
		return time.Time{}, fmt.Errorf("looking up %d: %w", pid, err)
	}

	if val == 0 {
		return time.Time{}, NoActivityRecordedErr{}
	}

	return convertBPFTime(val)
}

func (c *EBPFTracker) Close() error {
	return c.tcpEvents.Close()
}

// RemovePodIPv4 adds the pod IP to the tracker unless it already exists.
func (c *EBPFTracker) PutPodIP(ip netip.Addr) error {
	if ip.Is4() {
		val := uint32(0)
		ipv4 := ip.As4()
		uIP := binary.NativeEndian.Uint32(ipv4[:])
		if err := c.podKubeletAddrsv4.Update(&uIP, &val, ebpf.UpdateNoExist); err != nil &&
			!errors.Is(err, ebpf.ErrKeyExist) {
			return fmt.Errorf("unable to put ipv4 %s into bpf map: %w", ip, err)
		}
		return nil
	}

	val := bpfIpv6Addr{}
	bpfIP := bpfIpv6Addr{U6Addr8: ip.As16()}
	if err := c.podKubeletAddrsv6.Update(&bpfIP, &val, ebpf.UpdateNoExist); err != nil &&
		!errors.Is(err, ebpf.ErrKeyExist) {
		return fmt.Errorf("unable to put ipv6 %s into bpf map: %w", ip, err)
	}

	return nil
}

// RemovePodIPv4 removes the pod IP from the tracker.
func (c *EBPFTracker) RemovePodIP(ip netip.Addr) error {
	if ip.Is4() {
		ipv4 := ip.As4()
		uIP := binary.NativeEndian.Uint32(ipv4[:])
		if err := c.podKubeletAddrsv4.Delete(&uIP); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			return fmt.Errorf("unable to delete ipv4 %s from bpf map: %w", ip, err)
		}
		return nil
	}

	bpfIP := bpfIpv6Addr{U6Addr8: ip.As16()}
	if err := c.podKubeletAddrsv6.Delete(&bpfIP); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
		return fmt.Errorf("unable to delete ipv6 %s from bpf map: %w", ip, err)
	}
	return nil
}

// convertBPFTime takes the value of bpf_ktime_get_ns and converts it to a
// time.Time.
func convertBPFTime(t uint64) (time.Time, error) {
	b, err := getBootTimeNS()
	if err != nil {
		return time.Time{}, err
	}

	return time.Now().Add(-time.Duration(b - int64(t))), nil
}

// getKtimeNS returns the time elapsed since system boot, in nanoseconds. Does
// not include time the system was suspended. Basically the equivalent of
// bpf_ktime_get_ns.
func getBootTimeNS() (int64, error) {
	var ts unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	if err != nil {
		return 0, fmt.Errorf("could not get time: %s", err)
	}

	return unix.TimespecToNsec(ts), nil
}
