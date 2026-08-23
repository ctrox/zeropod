package reuse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
)

type forwarder struct {
	targetAddr     string
	connectTimeout time.Duration
	log            *log.Entry
	ln             net.Listener
	ns             ns.NetNS
	quit           chan struct{}
}

// ForwardToTarget creates a TCP proxy and replaces the app listener with it to
// forward traffic to the target addr.
// TODO: the tracker does not detect this traffic for some reason.
func (act *Activator) ForwardToTarget(ctx context.Context, addr string) error {
	act.log.Infof("starting forward to target %s", addr)
	act.mu.Lock()
	defer act.mu.Unlock()
	for k, ln := range act.listeners {
		fwd := &forwarder{
			targetAddr:     addr,
			log:            act.log.WithField("component", "forwarder"),
			ns:             act.ns,
			connectTimeout: act.connectTimeout,
			quit:           make(chan struct{}, 1),
		}
		if err := act.ns.Do(func(nn ns.NetNS) error {
			ln, err := listenReuseport(k.port, k.network, int(ln.app.uid))
			if err != nil {
				return err
			}
			fwd.ln = ln
			return nil
		}); err != nil {
			return err
		}
		if err := act.attachNetListener(fwd.ln, appKey, ln.reuse.Listeners, ln.reuse.SelectOrMigrate, nil); err != nil {
			return fmt.Errorf("registering listener: %w", err)
		}
		act.listeners[k].forwarder = *fwd
		go fwd.serveForward(ctx, fwd.ln, k.port)
	}
	return nil
}

func (fwd *forwarder) close() {
	if fwd.quit != nil {
		select {
		case fwd.quit <- struct{}{}:
		default:
		}
	}
	if fwd.ln != nil {
		_ = fwd.ln.Close()
	}
}

func (fwd *forwarder) serveForward(ctx context.Context, listener net.Listener, port uint16) {
	wg := sync.WaitGroup{}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			// TODO: we need this again? Or can we just use ctx?
			case <-fwd.quit:
				wg.Wait()
				fwd.log.Debug("quit")
				return
			case <-ctx.Done():
				wg.Wait()
				fwd.log.Debug("context closed")
				return
			default:
				if errors.Is(err, net.ErrClosed) {
					wg.Wait()
					fwd.log.Debug("listener closed")
					return
				}
				fwd.log.Errorf("error accepting: %s", err)
			}
		} else {
			wg.Go(func() {
				fwd.log.Debugf("accepting connection from %s", conn.RemoteAddr())
				fwd.handleForwardConn(ctx, conn, port)
			})
		}
	}
}

func (fwd *forwarder) handleForwardConn(ctx context.Context, conn net.Conn, port uint16) {
	backendConn, err := fwd.connect(ctx, port, fwd.targetAddr)
	if err != nil {
		log.G(ctx).Errorf("error establishing connection: %s", err)
		return
	}
	//nolint:errcheck
	defer backendConn.Close()

	requestContext, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := proxy(requestContext, conn, backendConn); err != nil {
		log.G(ctx).Errorf("error proxying request: %s", err)
	}
}

func (fwd *forwarder) connect(ctx context.Context, port uint16, addr string) (net.Conn, error) {
	var backendConn net.Conn
	dialer := net.Dialer{
		Timeout: fwd.connectTimeout,
	}
	targetAddr, err := net.ResolveTCPAddr("tcp", addr+":0")
	if err != nil {
		return nil, fmt.Errorf("parsing target addr: %w", err)
	}
	targetAddr.Port = int(port)
	// if we dial a remote target we want a smaller timeout as we might run
	// into an io timeout instead of connection refused
	dialer.Timeout = time.Millisecond * 10
	fwd.log.Debugf("connecting to target address %s", targetAddr.String())
	ticker := time.NewTicker(time.Millisecond)

	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Since(start) > fwd.connectTimeout {
				return nil, fmt.Errorf("timeout dialing process")
			}
			if err := fwd.ns.Do(func(_ ns.NetNS) error {
				var err error
				backendConn, err = dialer.Dial("tcp", targetAddr.String())
				return err
			}); err != nil {
				var serr syscall.Errno
				if errors.As(err, &serr) && serr == syscall.ECONNREFUSED {
					// executed program might not be ready yet, so retry in a bit.
					continue
				}
				var operr *net.OpError
				if errors.As(err, &operr) && operr.Temporary() {
					fwd.log.Errorf("temporary operr: %s", operr)
					continue
				}
				return nil, fmt.Errorf("unable to connect to process: %s", err)
			}

			return backendConn, nil
		}
	}
}

// proxy just proxies between conn1 and conn2.
func proxy(ctx context.Context, conn1, conn2 net.Conn) error {
	//nolint:errcheck
	defer conn1.Close()
	//nolint:errcheck
	defer conn2.Close()

	errors := make(chan error, 2)
	done := make(chan struct{}, 2)
	go cp(done, errors, conn2, conn1)
	go cp(done, errors, conn1, conn2)

	select {
	case <-ctx.Done():
		return nil
	case <-done:
		return nil
	case err := <-errors:
		return err
	}
}

func cp(done chan struct{}, errors chan error, dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	done <- struct{}{}
	if err != nil {
		errors <- err
	}
}
