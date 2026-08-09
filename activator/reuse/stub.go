package reuse

import "time"

func (act *Activator) DisableRedirects() error {
	return nil
}

func (act *Activator) AttachExec() error {
	return nil
}

func (act *Activator) SetProxyTimeout(d time.Duration) {}

func (act *Activator) SetConnectTimeout(d time.Duration) {}
