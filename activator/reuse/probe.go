package reuse

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ctrox/zeropod/activator"
)

type probeListener struct {
	ln net.Listener
}

// listenProbe will call listenReuseport and store the newly created listener in
// probe. Needs to be called inside the target network namespace.
func (act *Activator) listenProbe(port uint16, network activator.Network, lg *listenerGroup) error {
	ln, err := listenReuseport(port, network, int(lg.app.uid))
	if err != nil {
		return fmt.Errorf("wake listener: %w", err)
	}
	lg.probe.ln = ln
	return nil
}

func (act *Activator) attachProbe(lg *listenerGroup) error {
	go func() {
		for {
			conn, err := lg.probe.ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					break
				}
				act.log.WithError(err).Error("accepting probe connection")
				time.Sleep(time.Millisecond * 100)
				continue
			}
			tcpConn, ok := conn.(*net.TCPConn)
			if !ok {
				act.log.Errorf("probe connection is not a *net.TCPConn: %T", conn)
				_ = conn.Close()
				continue
			}
			if err := handleProbe(tcpConn); err != nil {
				act.log.WithError(err).Error("handling probe")
			}
		}
	}()
	return act.attachNetListener(lg.probe.ln, probeKey, lg.reuse.Listeners, lg.reuse.SelectOrMigrate, nil)
}

// handleProbe writes an HTTP response to conn that satisfies the kubelet and
// immediately closes the connection. It writes a raw http response to avoid
// importing net/http which inflates the shim.
func handleProbe(conn *net.TCPConn) error {
	_, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nServer: zeropod probe\r\nConnection: close\r\n\r\nok\n"))
	if err != nil {
		return fmt.Errorf("writing probe response: %w", err)
	}
	if err := conn.CloseWrite(); err != nil {
		return fmt.Errorf("closing write probe connection: %w", err)
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("closing probe connection: %w", err)
	}
	return err
}

func (pl *probeListener) closeListener() {
	if pl.ln != nil {
		_ = pl.ln.Close()
	}
}

func (pl *probeListener) close() {
	pl.closeListener()
}
