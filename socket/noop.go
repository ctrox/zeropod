package socket

import "time"

func NewNoopTracker(scaleDownDuration time.Duration) NoopTracker {
	return NoopTracker{
		PIDResolver:       noopResolver{},
		scaleDownDuration: scaleDownDuration,
	}
}

type NoopTracker struct {
	PIDResolver
	scaleDownDuration time.Duration
}

func (n NoopTracker) TrackPid(pid uint32) error {
	return nil
}

func (n NoopTracker) RemovePid(pid uint32) error {
	return nil
}

func (n NoopTracker) LastActivity(pid uint32) (time.Time, error) {
	return time.Now().Add(-n.scaleDownDuration), nil
}

func (n NoopTracker) Close() error {
	return nil
}

func (n NoopTracker) PutPodIP(ip uint32) error {
	return nil
}
