package node

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
)

type pageServerProxy struct {
	tlsConfig   *tls.Config
	log         *slog.Logger
	port        int
	backendAddr string
	listenAddr  string
	done        chan struct{}
	err         error
	listener    net.Listener
}

// newPageServerProxy returns a TCP proxy for use with a criu page server
// listening on a local unix socket. As the page server is one-shot, the proxy
// will automatically stop after the client has disconnected.
func newPageServerProxy(addr, backendAddr string, tlsConfig *tls.Config, log *slog.Logger) *pageServerProxy {
	psp := &pageServerProxy{
		log:         log.WithGroup("page-server-proxy"),
		tlsConfig:   tlsConfig,
		listenAddr:  addr,
		backendAddr: backendAddr,
		done:        make(chan struct{}),
	}
	return psp
}

func (p *pageServerProxy) listen(network, laddr string) (net.Listener, error) {
	var listener net.Listener
	var err error
	if p.tlsConfig != nil {
		listener, err = tls.Listen(network, laddr, p.tlsConfig)
	} else {
		listener, err = net.Listen(network, laddr)
	}
	if err != nil {
		return nil, err
	}

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("addr is not a net.TCPAddr: %T", listener.Addr())
	}
	p.port = addr.Port

	return listener, nil
}

func (p *pageServerProxy) Start(ctx context.Context) error {
	listener, err := p.listen("tcp", p.listenAddr)
	if err != nil {
		return err
	}
	p.listener = listener

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("addr is not a net.TCPAddr: %T", listener.Addr())
	}
	p.port = addr.Port

	go p.accept(ctx)
	go func() {
		<-ctx.Done()
		p.listener.Close()
	}()
	return nil
}

// Port returns the port the proxy is listening on. Only set after Start() has
// returned.
func (p *pageServerProxy) Port() int {
	return p.port
}

func (p *pageServerProxy) accept(ctx context.Context) {
	defer func() { p.done <- struct{}{} }()
	// as the page server is one-shot we only need to accept exactly once
	conn, err := p.listener.Accept()
	if err != nil {
		if ctx.Err() != nil {
			p.err = ctx.Err()
			return
		}
		p.err = err
		return
	}

	if err := p.HandleConn(ctx, conn); err != nil {
		p.log.Info("handling request", "error", err)
		p.err = err
	}
}

func (p *pageServerProxy) HandleConn(ctx context.Context, src net.Conn) error {
	target, err := net.Dial("unix", p.backendAddr)
	if err != nil {
		return fmt.Errorf("dialing target: %w", err)
	}
	p.log.Info("handling page server proxy connection", "remote_addr", src.RemoteAddr())
	conn, ok := src.(*tls.Conn)
	if !ok {
		return fmt.Errorf("expected a *tls.Conn, got %T", conn)
	}

	if err := conn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("error during handshake: %w", err)
	}
	p.log.Info("handshake complete", "tls_version", tls.VersionName(conn.ConnectionState().Version))
	if err := proxy(ctx, src, target); err != nil {
		return fmt.Errorf("proxy error: %w", err)
	}

	return nil
}

// Wait waits until the server is done handling a connection or the context is
// cancelled.
func (p *pageServerProxy) Wait() error {
	<-p.done
	p.log.Info("page server done")
	return p.err
}

// proxy just proxies between conn1 and conn2. If the ctx is cancelled, both
// sides of the connections are closed.
func proxy(ctx context.Context, conn1, conn2 net.Conn) error {
	defer conn1.Close()
	defer conn2.Close()

	done := make(chan struct{}, 1)
	errs := make(chan error, 1)
	go func() {
		copy(errs, conn1, conn2)
		done <- struct{}{}
	}()
	select {
	case err := <-errs:
		return err
	case <-done:
		return nil
	case <-ctx.Done():
		return nil
	}
}

func copy(errs chan error, conn1, conn2 net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, err := io.Copy(conn1, conn2); err != nil {
			errs <- err
		}
		conn1.(*tls.Conn).CloseWrite()
	}()
	go func() {
		defer wg.Done()
		if _, err := io.Copy(conn2, conn1); err != nil {
			errs <- err
		}
		conn2.(*net.UnixConn).CloseWrite()
	}()

	wg.Wait()
}
