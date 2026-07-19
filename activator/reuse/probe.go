package reuse

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
)

type probeListener struct {
	ln net.Listener
}

func (act *Activator) listenProbe(port uint16, network network, wl *wakeListener, pl *probeListener) error {
	if err := act.ns.Do(func(nn ns.NetNS) error {
		ln, err := listenReuseport(port, network)
		if err != nil {
			return fmt.Errorf("wake listener: %w", err)
		}
		pl.ln = ln
		return nil
	}); err != nil {
		return err
	}
	go func() {
		for {
			conn, err := pl.ln.Accept()
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
	return act.attachNetListener(pl.ln, probeKey, wl.reuse.Listeners, wl.reuse.SelectOrMigrate, nil)
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
