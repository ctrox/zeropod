package socket

import (
	"fmt"
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
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf kprobe.c -- -I/headers

const TCPEventsMap = "tcp_events"

// LoadEBPFTracker loads the eBPF program and attaches the kretprobe to track
// connections system-wide.
func LoadEBPFTracker() (func() error, error) {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, err
	}

	pinPath := activator.MapsPath()
	if err := os.MkdirAll(pinPath, os.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to create bpf fs subpath: %w", err)
	}

	// Load pre-compiled programs and maps into the kernel.
	objs := bpfObjects{}
	if err := loadBpfObjects(&objs, &ebpf.CollectionOptions{
		Maps: ebpf.MapOptions{
			// Pin the map to the BPF filesystem and configure the
			// library to automatically re-write it in the BPF
			// program so it can be re-used if it already exists or
			// create it if not.
			PinPath: pinPath,
		},
	}); err != nil {
		return nil, fmt.Errorf("loading objects: %w", err)
	}

	// in the past we used inet_sock_set_state here but we now use a
	// kretprobe with inet_csk_accept as inet_sock_set_state is not giving us
	// reliable PIDs. https://github.com/iovisor/bcc/issues/2304
	kp, err := link.Kretprobe("inet_csk_accept", objs.KretprobeInetCskAccept, &link.KprobeOptions{})
	if err != nil {
		return nil, fmt.Errorf("linking kprobe: %w", err)
	}

	return func() error {
		if err := objs.Close(); err != nil {
			return err
		}
		return kp.Close()
	}, nil
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

	tcpEvents, err := ebpf.LoadPinnedMap(filepath.Join(activator.MapsPath(), TCPEventsMap), &ebpf.LoadPinOptions{})

	return &EBPFTracker{
		PIDResolver: resolver,
		tcpEvents:   tcpEvents,
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
	tcpEvents *ebpf.Map
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
