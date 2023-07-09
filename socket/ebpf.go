package socket

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

const BPFFSPath = "/sys/fs/bpf"

// $BPF_CLANG and $BPF_CFLAGS are set by the Makefile.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc $BPF_CLANG -cflags $BPF_CFLAGS bpf kprobe.c -- -I/headers

// NewEBPFTracker returns a TCP connection tracker that will keep track of the
// last TCP accept of specific processes. It writes the results to an ebpf map
// keyed with the PID and the value contains the timestamp of the last
// observed accept.
func NewEBPFTracker() (Tracker, error) {
	// Allow the current process to lock memory for eBPF resources.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, err
	}

	pinPath := path.Join(BPFFSPath, "zeropod_maps")
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

	// in the past we used inet_sock_set_state here but we now we use a
	// kretprobe with inet_csk_accept as inet_sock_set_state is not giving us
	// reliable PIDs. https://github.com/iovisor/bcc/issues/2304
	kp, err := link.Kretprobe("inet_csk_accept", objs.KretprobeInetCskAccept, &link.KprobeOptions{})
	if err != nil {
		return nil, fmt.Errorf("linking kprobe: %w", err)
	}

	// TODO: pinning a perf event does not seem possible? What are the
	// implications here, will we run into issues if we have many probes
	// at the same time?
	// if err := kp.Pin(BPFFSPath); err != nil {
	// 	return nil, err
	// }

	var resolver PIDResolver
	resolver = noopResolver{}
	// if hostProcPath exists, we're probably running in a test container. We
	// will use the hostResolver instead of using the actual pids.
	if _, err := os.Stat(hostProcPath); err == nil {
		resolver = hostResolver{}
	}

	return &EBPFTracker{
		PIDResolver: resolver,
		objs:        objs,
		kp:          kp,
	}, nil
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
	objs bpfObjects
	kp   link.Link
	PIDResolver
}

// TrackPid puts the pid into the TcpEvents map meaning tcp events of the
// process belonging to that pid will be tracked.
func (c *EBPFTracker) TrackPid(pid uint32) error {
	val := uint64(0)
	pid = c.PIDResolver.Resolve(pid)
	if err := c.objs.TcpEvents.Put(&pid, &val); err != nil {
		return fmt.Errorf("unable to put pid %d into bpf map: %w", pid, err)
	}

	return nil
}

// RemovePid removes the pid from the TcpEvents map.
func (c *EBPFTracker) RemovePid(pid uint32) error {
	pid = c.PIDResolver.Resolve(pid)
	return c.objs.TcpEvents.Delete(&pid)
}

// LastActivity returns a time.Time of the last tcp activity recorded of the
// process belonging to the pid (or a child-process of the pid).
func (c *EBPFTracker) LastActivity(pid uint32) (time.Time, error) {
	var val uint64

	pid = c.PIDResolver.Resolve(pid)
	if err := c.objs.TcpEvents.Lookup(&pid, &val); err != nil {
		return time.Time{}, fmt.Errorf("looking up %d: %w", pid, err)
	}

	if val == 0 {
		return time.Time{}, NoActivityRecordedErr{}
	}

	return convertBPFTime(val)
}

func (c *EBPFTracker) Close() error {
	if err := c.objs.Close(); err != nil {
		return err
	}
	return c.kp.Close()
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

// MountBPFFS executes a mount -t bpf on the supplied path
func MountBPFFS(path string) error {
	return mount("bpf", "bpf", path)
}

// MountBPFFS mounts the kernel debugfs
func MountDebugFS() error {
	return mount("debugfs", "debugfs", "/sys/kernel/debug")
}

func mount(name, typ, path string) error {
	const alreadyMountedCode = 32
	out, err := exec.Command("mount", "-t", typ, name, path).CombinedOutput()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			if exitError.ExitCode() == alreadyMountedCode {
				return nil
			}
		}
		return fmt.Errorf("unable to mount BPF fs: %s: %s", err, out)
	}

	return nil
}
