package socket

import "time"

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
}
