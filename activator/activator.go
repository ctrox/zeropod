package activator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containernetworking/plugins/pkg/ns"
)

type Server struct {
	listeners      []net.Listener
	ports          []uint16
	quit           chan interface{}
	wg             sync.WaitGroup
	onAccept       OnAccept
	connectTimeout time.Duration
	proxyTimeout   time.Duration
	proxyCancel    context.CancelFunc
	ns             ns.NetNS
	firstAccept    sync.Once
	bpfCloseFunc   func()
	bpfObjs        *bpfObjects
}

type OnAccept func() error

func NewServer(ctx context.Context, ports []uint16, nn ns.NetNS, ifaces ...string) (*Server, error) {
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("no interfaces have been supplied, at least one is required")
	}

	s := &Server{
		quit:           make(chan interface{}),
		ports:          ports,
		connectTimeout: time.Second * 5,
		proxyTimeout:   time.Second * 5,
		ns:             nn,
	}

	if err := nn.Do(func(_ ns.NetNS) error {
		objs, close, err := initBPF(ifaces...)
		if err != nil {
			return err
		}
		s.bpfObjs = objs
		s.bpfCloseFunc = close
		return nil
	}); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Server) Start(ctx context.Context, onAccept OnAccept) error {
	for _, port := range s.ports {
		proxyPort, err := s.listen(ctx, port, onAccept)
		if err != nil {
			return err
		}

		log.G(ctx).Debugf("redirecting port %d -> %d", port, proxyPort)
		if err := s.RedirectPort(port, uint16(proxyPort)); err != nil {
			return fmt.Errorf("redirecting port: %w", err)
		}
	}

	return nil
}

func (s *Server) Reset() error {
	s.firstAccept = sync.Once{}
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

func (s *Server) listen(ctx context.Context, port uint16, onAccept OnAccept) (int, error) {
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

	s.firstAccept = sync.Once{}
	s.onAccept = onAccept

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

	s.bpfCloseFunc()

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
				s.handleConection(ctx, conn, port)
				wg.Done()
			}()
		}
	}
}

func (s *Server) handleConection(ctx context.Context, conn net.Conn, port uint16) {
	defer conn.Close()

	tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		log.G(ctx).Errorf("unable to get TCP Addr from remote addr: %T", conn.RemoteAddr())
		return
	}

	log.G(ctx).Debugf("registering connection on remote port %d", tcpAddr.Port)
	if err := s.registerConnection(uint16(tcpAddr.Port)); err != nil {
		log.G(ctx).Errorf("error registering connection: %s", err)
		return
	}

	s.firstAccept.Do(func() {
		if err := s.onAccept(); err != nil {
			log.G(ctx).Errorf("accept function: %s", err)
			return
		}
	})

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
		log.G(ctx).Errorf("error removing connection: %s", err)
		return
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
