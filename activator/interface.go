package activator

import (
	"context"
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
