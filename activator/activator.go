package activator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
)

type Server struct {
	listener net.Listener
	port     int
	quit     chan interface{}
	wg       sync.WaitGroup
	hook     hookFunc
}

type hookFunc func(f *os.File) error

func NewServer(ctx context.Context, port int) *Server {
	s := &Server{
		quit: make(chan interface{}),
		port: port,
	}

	return s
}

func (s *Server) Start(ctx context.Context, hook hookFunc) error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.port)
	cfg := net.ListenConfig{}
	l, err := cfg.Listen(ctx, "tcp4", addr)
	if err != nil {
		return err
	}

	log.G(ctx).Infof("listening on %s", addr)

	s.hook = hook
	s.listener = l
	s.wg.Add(1)
	go s.serve(ctx)
	return nil
}

func (s *Server) Stop() {
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
}

func (s *Server) handleConection(ctx context.Context, conn net.Conn) {
	// we close our listener after accepting the first connection so it's free
	// to use for the to-be-activated program.
	// TODO: test what happens with concurrent connections, do we get a
	// connection refused if a connection happens between this and starting
	// the child process?
	s.listener.Close()

	// retrieve copy of connection file descriptor
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		log.G(ctx).Fatalf("failed to cast connection to TCP connection")
	}
	f, err := tcpConn.File()
	if err != nil {
		log.G(ctx).Fatalf("failed to retrieve copy of the underlying TCP connection file")
	}
	defer tcpConn.Close()

	if err := s.hook(f); err != nil {
		log.G(ctx).Fatal(err)
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
				if time.Since(start) > time.Second*5 {
					log.G(ctx).Error("timeout dialing process")

					tcpConn.Close()
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

	if err := pipe(tcpConn, initialConn); err != nil {
		log.G(ctx).Fatal(err)
	}

	log.G(ctx).Println("initial connection closed")
}

// pipe pipes two net.Conn and blocks until one of them reaches an error or
// EOF. Adapted from https://www.stavros.io/posts/proxying-two-connections-go/
func pipe(conn1 net.Conn, conn2 net.Conn) error {
	errChan := make(chan error)
	chan1 := chanFromConn(conn1, errChan)
	chan2 := chanFromConn(conn2, errChan)

	defer close(errChan)
	defer close(chan1)
	defer close(chan2)

	for {
		select {
		case b1 := <-chan1:
			if _, err := conn2.Write(b1); err != nil {
				return err
			}
		case b2 := <-chan2:
			if _, err := conn1.Write(b2); err != nil {
				return err
			}
		case err := <-errChan:
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// chanFromConn creates a channel from a Conn object, and sends everything it
// Read()s from the socket to the channel.
func chanFromConn(conn net.Conn, errChan chan error) chan []byte {
	c := make(chan []byte)

	go func() {
		b := make([]byte, 1024)

		for {
			n, err := conn.Read(b)
			if n > 0 {
				res := make([]byte, n)
				// Copy the buffer so it doesn't get changed while read by the recipient.
				copy(res, b[:n])
				c <- res
			}
			if err != nil {
				// TODO: this can panic (send on closed channel)
				errChan <- err
				break
			}
		}
	}()

	return c
}
