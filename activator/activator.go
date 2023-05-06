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
	"github.com/ctrox/zeropod/process"
	"github.com/ctrox/zeropod/runc"
)

type Server struct {
	listener       net.Listener
	port           int
	quit           chan interface{}
	wg             sync.WaitGroup
	onAccept       onAcceptFunc
	onClosed       onClosedFunc
	connectTimeout time.Duration
}

type onAcceptFunc func() (*runc.Container, process.Process, error)
type onClosedFunc func(*runc.Container, process.Process) error

func NewServer(ctx context.Context, port int) *Server {
	s := &Server{
		quit:           make(chan interface{}),
		port:           port,
		connectTimeout: time.Second * 5,
	}

	return s
}

func (s *Server) Start(ctx context.Context, onAccept onAcceptFunc, onClosed onClosedFunc) error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.port)
	cfg := net.ListenConfig{}
	l, err := cfg.Listen(ctx, "tcp4", addr)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("listening on %s", addr)

	s.onAccept = onAccept
	s.onClosed = onClosed
	s.listener = l
	s.wg.Add(1)
	go s.serve(ctx)
	return nil
}

func (s *Server) Stop(ctx context.Context) {
	log.G(ctx).Info("stopping activator")
	close(s.quit)
	s.listener.Close()
	s.wg.Wait()
}

func (s *Server) serve(ctx context.Context) {
	defer s.wg.Done()

	conn, err := s.listener.Accept()
	if err != nil {
		select {
		case <-s.quit:
			return
		case <-ctx.Done():
			return
		default:
			log.G(ctx).Errorf("error accepting: %s", err)
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

func (s *Server) handleConection(ctx context.Context, conn net.Conn) {
	// we close our listener after accepting the first connection so it's free
	// to use for the to-be-activated program.
	// TODO: test what happens with concurrent connections, do we get a
	// connection refused if a connection happens between this and starting
	// the child process?
	if err := s.listener.Close(); err != nil {
		log.G(ctx).Errorf("error during listener close: %s", err)
	}
	defer conn.Close()

	c, p, err := s.onAccept()
	if err != nil {
		log.G(ctx).Error(err)
		return
	}

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

				initialConn, err = net.DialTimeout("tcp4", fmt.Sprintf("localhost:%d", s.port), 5*time.Second)
				if err != nil {
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

	if err := s.onClosed(c, p); err != nil {
		log.G(ctx).Error(err)
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
