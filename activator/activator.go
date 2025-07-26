// Package activator contains a userspace TCP proxy that listens on a random
// port and loads an eBPF program to intercept and redirect packets destined to
// the configured ports. The activator accepts the connection, calls onAccept,
// signals to disable the eBPF redirect and then proxies the initial data to the
// defined ports as soon as something is listening.
package activator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
)

type Server struct {
	listeners      []net.Listener
	ports          []uint16
	quit           chan any
	wg             sync.WaitGroup
	connHook       ConnHook
	restoreHook    RestoreHook
	connectTimeout time.Duration
	proxyTimeout   time.Duration
	proxyCancel    context.CancelFunc
	ns             ns.NetNS
	maps           bpfMaps
	sandboxPid     int
	started        bool
	peekBufferSize int
}

type ConnHook func(net.Conn) (conn net.Conn, cont bool, err error)
type RestoreHook func() error

func NewServer(ctx context.Context, nn ns.NetNS) (*Server, error) {
	s := &Server{
		quit:           make(chan any),
		connectTimeout: time.Second * 5,
		proxyTimeout:   time.Second * 5,
		ns:             nn,
		sandboxPid:     parsePidFromNetNS(nn),
	}

	return s, os.MkdirAll(PinPath(s.sandboxPid), os.ModePerm)
}

func parsePidFromNetNS(nn ns.NetNS) int {
	parts := strings.Split(nn.Path(), "/")
	if len(parts) < 3 {
		return 0
	}

	pid, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0
	}

	return pid
}

var ErrMapNotFound = errors.New("bpf map could not be found")

func (s *Server) Start(ctx context.Context, ports []uint16, connHook ConnHook, restoreHook RestoreHook) error {
	s.ports = ports
	s.connHook = connHook
	s.restoreHook = restoreHook

	if err := s.loadPinnedMaps(); err != nil {
		return err
	}

	for _, port := range s.ports {
		proxyPort, err := s.listen(ctx, port)
		if err != nil {
			return err
		}

		log.G(ctx).Debugf("redirecting port %d -> %d", port, proxyPort)
		if err := s.RedirectPort(port, uint16(proxyPort)); err != nil {
			return fmt.Errorf("redirecting port: %w", err)
		}
	}

	s.started = true
	return nil
}

func (s *Server) Started() bool {
	return s.started
}

func (s *Server) Reset() error {
	for _, port := range s.ports {
		if err := s.enableRedirect(port); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) DisableRedirects() error {
	for _, port := range s.ports {
		if err := s.disableRedirect(port); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) SetPeekBufferSize(size int) {
	s.peekBufferSize = size
}

func (s *Server) listen(ctx context.Context, port uint16) (int, error) {
	// use a random free port for our proxy
	addr := "0.0.0.0:0"
	cfg := net.ListenConfig{}

	var listener net.Listener
	if err := s.ns.Do(func(_ ns.NetNS) error {
		l, err := cfg.Listen(ctx, "tcp4", addr)
		if err != nil {
			return fmt.Errorf("unable to listen: %w", err)
		}

		listener = l
		s.listeners = append(s.listeners, l)
		return nil
	}); err != nil {
		return 0, err
	}

	log.G(ctx).Debugf("listening on %s in ns %s", listener.Addr(), s.ns.Path())

	s.wg.Add(1)
	go s.serve(ctx, listener, port)

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unable to get TCP Addr from remote addr: %T", listener.Addr())
	}

	return tcpAddr.Port, nil
}

func (s *Server) Stop(ctx context.Context) {
	log.G(ctx).Debugf("stopping activator")

	if s.proxyCancel != nil {
		s.proxyCancel()
	}

	for _, l := range s.listeners {
		l.Close()
	}

	log.G(ctx).Debugf("removing %s", PinPath(s.sandboxPid))

	_ = os.RemoveAll(PinPath(s.sandboxPid))

	s.wg.Wait()
	log.G(ctx).Debugf("activator stopped")
}

func (s *Server) serve(ctx context.Context, listener net.Listener, port uint16) {
	defer s.wg.Done()
	wg := sync.WaitGroup{}

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				wg.Wait()
				return
			case <-ctx.Done():
				wg.Wait()
				return
			default:
				if !errors.Is(err, net.ErrClosed) {
					log.G(ctx).Errorf("error accepting: %s", err)
				}
				return
			}
		} else {
			wg.Add(1)
			go func() {
				log.G(ctx).Debug("accepting connection")
				s.handleConnection(ctx, conn, port)
				wg.Done()
			}()
		}
	}
}

func (s *Server) handleConnection(ctx context.Context, netConn net.Conn, port uint16) {
	conn, cont, err := s.connHook(netConn)
	if err != nil {
		log.G(ctx).Errorf("connHook: %s", err)
		return
	}
	defer conn.Close()
	if !cont {
		return
	}

	tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		log.G(ctx).Errorf("unable to get TCP Addr from remote addr: %T", conn.RemoteAddr())
		return
	}

	log.G(ctx).Debugf("registering connection on remote port %d from %s", tcpAddr.Port, tcpAddr.IP.String())
	if err := s.registerConnection(uint16(tcpAddr.Port)); err != nil {
		log.G(ctx).Errorf("error registering connection: %s", err)
		return
	}

	if err := s.restoreHook(); err != nil {
		log.G(ctx).Errorf("restoreHook: %s", err)
		return
	}

	backendConn, err := s.connect(ctx, port)
	if err != nil {
		log.G(ctx).Errorf("error establishing connection: %s", err)
		return
	}
	defer backendConn.Close()

	log.G(ctx).Println("dial succeeded", backendConn.RemoteAddr().String())

	requestContext, cancel := context.WithTimeout(ctx, s.proxyTimeout)
	s.proxyCancel = cancel
	defer cancel()
	if err := proxy(requestContext, conn, backendConn); err != nil {
		log.G(ctx).Errorf("error proxying request: %s", err)
	}

	if err := s.removeConnection(uint16(tcpAddr.Port)); err != nil {
		log.G(ctx).Warnf("error removing connection: %s", err)
	}

	log.G(ctx).Println("connection closed", conn.RemoteAddr().String())
}

func (s *Server) connect(ctx context.Context, port uint16) (net.Conn, error) {
	var backendConn net.Conn

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Since(start) > s.connectTimeout {
				return nil, fmt.Errorf("timeout dialing process")
			}

			if err := s.ns.Do(func(_ ns.NetNS) error {
				// to ensure we don't create a redirect loop we need to know
				// the local port of our connection to the activated process.
				// We reserve a free port, store it in the disable bpf map and
				// then use it to make the connection.
				backendConnPort, err := freePort()
				if err != nil {
					return fmt.Errorf("unable to get free port: %w", err)
				}

				log.G(ctx).Debugf("registering backend connection port %d in bpf map", backendConnPort)
				if err := s.disableRedirect(uint16(backendConnPort)); err != nil {
					return err
				}

				addr, err := net.ResolveTCPAddr("tcp4", fmt.Sprintf("127.0.0.1:%d", backendConnPort))
				if err != nil {
					return err
				}
				d := net.Dialer{
					LocalAddr: addr,
					Timeout:   s.connectTimeout,
				}
				backendConn, err = d.Dial("tcp4", fmt.Sprintf("localhost:%d", port))
				return err
			}); err != nil {
				var serr syscall.Errno
				if errors.As(err, &serr) && serr == syscall.ECONNREFUSED {
					// executed program might not be ready yet, so retry in a bit.
					continue
				}
				return nil, fmt.Errorf("unable to connect to process: %s", err)
			}

			return backendConn, nil
		}
	}
}

const (
	activeConnectionsMap = "active_connections"
	disableRedirectMap   = "disable_redirect"
	egressRedirectsMap   = "egress_redirects"
	ingressRedirectsMap  = "ingress_redirects"
)

func (s *Server) loadPinnedMaps() error {
	// either all or none of the maps are pinned, so we want to return
	// ErrMapNotFound so it can be handled.
	if _, err := os.Stat(filepath.Join(PinPath(s.sandboxPid), activeConnectionsMap)); os.IsNotExist(err) {
		return ErrMapNotFound
	}

	var err error
	opts := &ebpf.LoadPinOptions{}
	if s.maps.ActiveConnections == nil {
		s.maps.ActiveConnections, err = ebpf.LoadPinnedMap(s.mapPath(activeConnectionsMap), opts)
		if err != nil {
			return err
		}
	}

	if s.maps.DisableRedirect == nil {
		s.maps.DisableRedirect, err = ebpf.LoadPinnedMap(s.mapPath(disableRedirectMap), opts)
		if err != nil {
			return err
		}
	}

	if s.maps.EgressRedirects == nil {
		s.maps.EgressRedirects, err = ebpf.LoadPinnedMap(s.mapPath(egressRedirectsMap), opts)
		if err != nil {
			return err
		}
	}

	if s.maps.IngressRedirects == nil {
		s.maps.IngressRedirects, err = ebpf.LoadPinnedMap(s.mapPath(ingressRedirectsMap), opts)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) mapPath(name string) string {
	return filepath.Join(PinPath(s.sandboxPid), name)
}

// RedirectPort redirects the port from to on ingress and to from on egress.
func (s *Server) RedirectPort(from, to uint16) error {
	if err := s.maps.IngressRedirects.Put(&from, &to); err != nil {
		return fmt.Errorf("unable to put ports %d -> %d into bpf map: %w", from, to, err)
	}
	if err := s.maps.EgressRedirects.Put(&to, &from); err != nil {
		return fmt.Errorf("unable to put ports %d -> %d into bpf map: %w", to, from, err)
	}
	return nil
}

func (s *Server) registerConnection(port uint16) error {
	if err := s.maps.ActiveConnections.Put(&port, uint8(1)); err != nil {
		return fmt.Errorf("unable to put port %d into bpf map: %w", port, err)
	}
	return nil
}

func (s *Server) removeConnection(port uint16) error {
	if err := s.maps.ActiveConnections.Delete(&port); err != nil {
		return fmt.Errorf("unable to delete port %d in bpf map: %w", port, err)
	}
	return nil
}

func (s *Server) disableRedirect(port uint16) error {
	if err := s.maps.DisableRedirect.Put(&port, uint8(1)); err != nil {
		return fmt.Errorf("unable to put %d into bpf map: %w", port, err)
	}
	return nil
}

func (s *Server) enableRedirect(port uint16) error {
	if err := s.maps.DisableRedirect.Delete(&port); err != nil {
		if !errors.Is(err, ebpf.ErrKeyNotExist) {
			return err
		}
	}
	return nil
}

// proxy just proxies between conn1 and conn2.
func proxy(ctx context.Context, conn1, conn2 net.Conn) error {
	defer conn1.Close()
	defer conn2.Close()

	errors := make(chan error, 2)
	done := make(chan struct{}, 2)
	go copy(done, errors, conn2, conn1)
	go copy(done, errors, conn1, conn2)

	select {
	case <-ctx.Done():
		log.G(ctx).Printf("context done with: %s", ctx.Err())
		return nil
	case <-done:
		return nil
	case err := <-errors:
		return err
	}
}

func copy(done chan struct{}, errors chan error, dst io.Writer, src io.Reader) {
	_, err := io.Copy(dst, src)
	done <- struct{}{}
	if err != nil {
		errors <- err
	}
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("addr is not a net.TCPAddr: %T", listener.Addr())
	}

	if err := listener.Close(); err != nil {
		return 0, err
	}

	return addr.Port, nil
}
