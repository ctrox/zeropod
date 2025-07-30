package socket

import (
	"net/netip"
	"time"
)

type Tracker interface {
	PIDResolver

	// TrackPid starts connection tracking of the specified process.
	TrackPid(pid uint32) error
	// TrackPid stops connection tracking of the specified process.
	RemovePid(pid uint32) error
	// LastActivity returns the time of the last TCP activity of the specified process.
	LastActivity(pid uint32) (time.Time, error)
	// Close the activity tracker.
	Close() error
	// PutPodIP inserts a pod IP into the pod-to-kubelet map, helping with
	// ignoring probes coming from kubelet within the tracker.
	PutPodIP(ip netip.Addr) error
	// RemovePodIP removes a pod IP from the pod-to-kubelet map.
	RemovePodIP(ip netip.Addr) error
}
