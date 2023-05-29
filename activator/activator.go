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
	listener       net.Listener
	port           uint16
	quit           chan interface{}
	wg             sync.WaitGroup
	onAccept       AcceptFunc
	onClosed       ClosedFunc
	connectTimeout time.Duration
	ns             ns.NetNS
	restore        sync.Once
}

type AcceptFunc func() (*runc.Container, error)
type ClosedFunc func(*runc.Container) error

func NewServer(ctx context.Context, port uint16, ns ns.NetNS) (*Server, error) {
	s := &Server{
		quit:           make(chan interface{}),
		port:           port,
		connectTimeout: time.Second * 5,
		ns:             ns,
	}

	return s, nil
}

func (s *Server) Start(ctx context.Context, onAccept AcceptFunc, onClosed ClosedFunc) error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.port)
	cfg := net.ListenConfig{}

	// for some reason, sometimes the address will still be in use after
	// checkpointing, so we wrap the listen in a retry.
	if err := backoff.Retry(
		func() error {
			// make sure to run the listener in our target namespace
			return s.ns.Do(func(_ ns.NetNS) error {
				l, err := cfg.Listen(ctx, "tcp4", addr)
				if err != nil {
					return fmt.Errorf("unable to listen: %w", err)
				}

				s.listener = l
				return nil
			})
		},
		newBackOff(),
	); err != nil {
		return err
	}

	log.G(ctx).Infof("listening on %s", addr)

	s.restore = sync.Once{}
	s.onAccept = onAccept
	s.onClosed = onClosed

	s.wg.Add(1)
	go s.serve(ctx)
	return nil
}

func (s *Server) Stop(ctx context.Context) {
	log.G(ctx).Info("stopping activator")
	if s.listener != nil {
		s.listener.Close()
	}
}

func (s *Server) serve(ctx context.Context) {
	defer s.wg.Done()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.quit:
				return
			case <-ctx.Done():
				return
			default:
				if !errors.Is(err, net.ErrClosed) {
					log.G(ctx).Errorf("error accepting: %s", err)
				}
				return
			}
		} else {
			s.wg.Add(1)
			go func() {
				log.G(ctx).Info("accepting connection")
				s.handleConection(ctx, conn)
				s.wg.Done()
			}()
		}
		log.G(ctx).Info("serve done")
	}
}

func (s *Server) handleConection(ctx context.Context, conn net.Conn) {
	// we close our listener after accepting the first connection so it's free
	// to use for the to-be-activated program.
	// TODO: test what happens with concurrent connections, do we get a
	// connection refused if a connection happens between this and starting
	// the child process?
	defer conn.Close()

	var c *runc.Container
	var err error

	s.restore.Do(func() {
		if err := s.listener.Close(); err != nil {
			log.G(ctx).Errorf("error during listener close: %s", err)
		}

		c, err = s.onAccept()
		if err != nil {
			log.G(ctx).Error(err)
			return
		}
	})

	log.G(ctx).Println("proxying initial connection to program")

	// fork is done but we need to finish up the initial connection. We do
	// this by connecting to our forked process and piping the tcpConn that we
	// initially accpted.
	var initialConn net.Conn

	ticker := time.NewTicker(time.Millisecond)
	start := time.Now()
	done := make(chan bool)

	go func() {
		for {
			select {
			case <-ctx.Done():
				log.G(ctx).Println("context cancelled")
				done <- true
				return
			case <-done:
				log.G(ctx).Println("done")
				return
			case <-ticker.C:
				if time.Since(start) > s.connectTimeout {
					log.G(ctx).Error("timeout dialing process")

					done <- true
					return
				}

				if err := s.ns.Do(func(_ ns.NetNS) error {
					initialConn, err = net.DialTimeout("tcp4", fmt.Sprintf("localhost:%d", s.port), s.connectTimeout)
					return err
				}); err != nil {
					var serr syscall.Errno
					if errors.As(err, &serr) && serr == syscall.ECONNREFUSED {
						// executed program might not be ready yet, so retry in a bit.
						// TODO: do this with an exponential backoff and timeout.
						continue
					}
					log.G(ctx).Errorf("unable to connect to process: %s", err)
					return
				}

				log.G(ctx).Println("dial succeeded")
				done <- true
				return
			}
		}
	}()

	<-done

	if initialConn == nil {
		return
	}

	defer initialConn.Close()

	proxy(conn, initialConn)

	log.G(ctx).Println("initial connection closed")

	if c != nil {
		if err := s.onClosed(c); err != nil {
			log.G(ctx).Error(err)
		}
	}
}

// proxy just proxies between conn1 and conn2.
// TODO: add timeout
func proxy(conn1, conn2 net.Conn) error {
	defer conn1.Close()
	defer conn2.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(conn1, conn2)
		// Signal peer that no more data is coming.
		conn1.Close()
	}()
	go func() {
		defer wg.Done()
		io.Copy(conn2, conn1)
		// Signal peer that no more data is coming.
		conn2.Close()
	}()

	wg.Wait()
	return nil
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
