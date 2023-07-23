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

	"github.com/cenkalti/backoff/v4"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/runtime/v2/runc"
	"github.com/containernetworking/plugins/pkg/ns"
)

type Server struct {
	listeners      []net.Listener
	ports          []uint16
	quit           chan interface{}
	wg             sync.WaitGroup
	onAccept       OnAccept
	onClosed       OnClosed
	connectTimeout time.Duration
	proxyTimeout   time.Duration
	proxyCancel    context.CancelFunc
	ns             ns.NetNS
	once           sync.Once
	Network        NetworkLocker
}

type OnAccept func() (*runc.Container, error)
type OnClosed func(*runc.Container) error

func NewServer(ctx context.Context, ports []uint16, ns ns.NetNS, locker NetworkLocker) (*Server, error) {
	s := &Server{
		quit:           make(chan interface{}),
		ports:          ports,
		connectTimeout: time.Second * 5,
		proxyTimeout:   time.Second * 5,
		ns:             ns,
		Network:        locker,
	}

	return s, nil
}

func (s *Server) Start(ctx context.Context, onAccept OnAccept, onClosed OnClosed) error {
	for _, port := range s.ports {
		if err := s.listen(ctx, port, onAccept, onClosed); err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) listen(ctx context.Context, port uint16, onAccept OnAccept, onClosed OnClosed) error {
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	cfg := net.ListenConfig{}

	// for some reason, sometimes the address will still be in use after
	// checkpointing, so we wrap the listen in a retry.
	var listener net.Listener
	if err := backoff.Retry(
		func() error {
			// make sure to run the listener in our target namespace
			return s.ns.Do(func(_ ns.NetNS) error {
				l, err := cfg.Listen(ctx, "tcp4", addr)
				if err != nil {
					return fmt.Errorf("unable to listen: %w", err)
				}

				listener = l
				s.listeners = append(s.listeners, l)
				return nil
			})
		},
		newBackOff(),
	); err != nil {
		return err
	}

	log.G(ctx).Infof("listening on %s in ns %s", addr, s.ns.Path())

	s.once = sync.Once{}
	s.onAccept = onAccept
	s.onClosed = onClosed

	s.wg.Add(1)
	go s.serve(ctx, listener, port)
	return nil
}

func (s *Server) Stop(ctx context.Context) {
	log.G(ctx).Info("stopping activator")

	if s.proxyCancel != nil {
		s.proxyCancel()
	}

	for _, l := range s.listeners {
		l.Close()
	}

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
	// we close our listener after accepting the first connection so it's free
	// to use for the to-be-activated program.

	// TODO: there is still a small chance a TCP connection ends up timing out
	// as there might be a backlog of already established connections that are
	// killed when we close the listener. Not sure if it's even possible to
	// fix that. It's reproducible by running TestActivator a bunch of times
	// (something like -count=50).
	defer conn.Close()

	var container *runc.Container
	var err error

	s.once.Do(func() {
		// we lock the network, close the listener, call onAccept and unlock only for
		// the first connection we get.
		beforeLock := time.Now()
		if err := s.Network.Lock([]uint16{port}); err != nil {
			log.G(ctx).Errorf("error locking network: %s", err)
			return
		}
		log.G(ctx).Printf("took %s to lock network", time.Since(beforeLock))

		var closeErr error
		for _, listener := range s.listeners {
			closeErr = listener.Close()
		}
		if closeErr != nil {
			log.G(ctx).Errorf("error during listener close: %s", closeErr)
		}

		container, err = s.onAccept()
		if err != nil {
			log.G(ctx).Errorf("error during onAccept: %s", err)
			return
		}

		beforeUnlock := time.Now()
		if err := s.Network.Unlock([]uint16{port}); err != nil {
			log.G(ctx).Errorf("error unlocking network: %s", err)
			return
		}
		log.G(ctx).Printf("took %s to unlock network", time.Since(beforeUnlock))
	})

	log.G(ctx).Println("proxying initial connection to program", conn.RemoteAddr().String())

	// fork is done but we need to finish up the initial connection. We do
	// this by connecting to our forked process and piping the tcpConn that we
	// initially accpted.
	initialConn, err := s.connect(ctx, port)
	if err != nil {
		log.G(ctx).Errorf("error establishing initial connection: %s", err)
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

	log.G(ctx).Println("initial connection closed", conn.RemoteAddr().String())

	if container != nil {
		if err := s.onClosed(container); err != nil {
			log.G(ctx).Error(err)
		}
	}
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

func newBackOff() *backoff.ExponentialBackOff {
	b := &backoff.ExponentialBackOff{
		InitialInterval:     time.Millisecond,
		RandomizationFactor: backoff.DefaultRandomizationFactor,
		Multiplier:          backoff.DefaultMultiplier,
		MaxInterval:         100 * time.Millisecond,
		MaxElapsedTime:      300 * time.Millisecond,
		Stop:                backoff.Stop,
		Clock:               backoff.SystemClock,
	}
	b.Reset()
	return b
}
