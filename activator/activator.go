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

func NewServer(ctx context.Context, ports []uint16, nn ns.NetNS) (*Server, error) {
	s := &Server{
		quit:           make(chan interface{}),
		ports:          ports,
		connectTimeout: time.Second * 5,
		proxyTimeout:   time.Second * 5,
		ns:             nn,
	}

	if err := nn.Do(func(_ ns.NetNS) error {
		// TODO: is this really always eth0?
		// we need loopback for port-forwarding to work
		objs, close, err := initBPF("lo", "eth0")
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

		log.G(ctx).Infof("redirecting port %d -> %d", port, proxyPort)
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

	// for some reason, sometimes the address will still be in use after
	// checkpointing, so we wrap the listen in a retry.
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

	log.G(ctx).Infof("listening on %s in ns %s", listener.Addr(), s.ns.Path())

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
	log.G(ctx).Info("stopping activator")

	if s.proxyCancel != nil {
		s.proxyCancel()
	}

	for _, l := range s.listeners {
		l.Close()
	}

	s.bpfCloseFunc()

	s.wg.Wait()
	log.G(ctx).Info("activator stopped")
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
				log.G(ctx).Info("accepting connection")
				s.handleConection(ctx, conn, port)
				wg.Done()
			}()
		}
	}
}

func (s *Server) handleConection(ctx context.Context, conn net.Conn, port uint16) {
	defer conn.Close()

	s.firstAccept.Do(func() {
		if err := s.onAccept(); err != nil {
			log.G(ctx).Errorf("accept function: %s", err)
			return
		}
	})

	tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		log.G(ctx).Errorf("unable to get TCP Addr from remote addr: %T", conn.RemoteAddr())
		return
	}

	log.G(ctx).Infof("registering connection on port %d", tcpAddr.Port)
	if err := s.registerConnection(uint16(tcpAddr.Port)); err != nil {
		log.G(ctx).Errorf("error registering fade out port: %s", err)
	}

	log.G(ctx).Printf("proxying connection to program at localhost:%d", port)

	initialConn, err := s.connect(ctx, port)
	if err != nil {
		log.G(ctx).Errorf("error establishing connection: %s", err)
		return
	}
	defer initialConn.Close()

	log.G(ctx).Println("dial succeeded", initialConn.RemoteAddr().String())

	requestContext, cancel := context.WithTimeout(ctx, s.proxyTimeout)
	s.proxyCancel = cancel
	defer cancel()
	if err := proxy(requestContext, conn, initialConn); err != nil {
		log.G(ctx).Errorf("error proxying request: %s", err)
	}

	log.G(ctx).Println("connection closed", conn.RemoteAddr().String())
}

func (s *Server) connect(ctx context.Context, port uint16) (net.Conn, error) {
	var initialConn net.Conn
	var err error

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
				initialConn, err = net.DialTimeout("tcp4", fmt.Sprintf("localhost:%d", port), s.connectTimeout)
				return err
			}); err != nil {
				var serr syscall.Errno
				if errors.As(err, &serr) && serr == syscall.ECONNREFUSED {
					// executed program might not be ready yet, so retry in a bit.
					continue
				}
				return nil, fmt.Errorf("unable to connect to process: %s", err)
			}

			return initialConn, nil
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
