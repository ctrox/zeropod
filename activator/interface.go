package activator

import (
	"context"
	"maps"
	"slices"
	"time"
)

type Activator interface {
	Start(ctx context.Context, pid int, listeners Listeners, skipStart bool) error
	Started() bool
	Reset() error
	DisableRedirects() error
	AttachExec() error
	SetProxyTimeout(d time.Duration)
	SetConnectTimeout(d time.Duration)
	LastActivity(port uint16) (time.Time, error)
	Stop(ctx context.Context)
	GetListeners() []Listener
	ForwardToTarget(ctx context.Context, addr string) error
}

type Network string

const (
	NetworkTCP4     Network = "tcp4"
	NetworkTCPAny   Network = "tcp"
	NetworkTCP6ONLY Network = "tcp6"
)

type Listener struct {
	Port    uint16
	Network Network
}

type Listeners []Listener

func (lns Listeners) Ports() []uint16 {
	ports := map[uint16]struct{}{}
	for _, ln := range lns {
		ports[ln.Port] = struct{}{}
	}
	return slices.Collect(maps.Keys(ports))
}
