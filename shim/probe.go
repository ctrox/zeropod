package shim

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/containerd/log"
	"github.com/ctrox/zeropod/activator"
)

// defaultProbeBufferSize should be able to fit kube-probe HTTP requests with
// reasonable path and header sizes but should still be small enough to not
// impact performance.
const defaultProbeBufferSize = 1024

func (c *Container) detectProbe(ctx context.Context) activator.ConnHook {
	if c.cfg.DisableProbeDetection {
		return func(conn net.Conn) (net.Conn, bool, error) {
			return conn, true, nil
		}
	}
	return func(netConn net.Conn) (net.Conn, bool, error) {
		conn := newBufferedConn(netConn, c.cfg.ProbeBufferSize)
		if isTCPProbe(ctx, conn) {
			log.G(ctx).Debug("detected TCP kube-probe, ignoring connection")
			return conn, false, nil
		}
		if isHTTPProbe(ctx, conn) {
			log.G(ctx).Debug("detected HTTP kube-probe request, responding")
			if err := probeResponse(conn); err != nil {
				log.G(ctx).Errorf("responding to kube-probe: %s", err)
			}
			return conn, false, nil
		}
		return conn, true, nil
	}
}

// isTCPProbe detects a TCP probe. It peeks 1 byte into the connection and if it
// receives an immediate [io.EOF] we know the conn has already been closed
// without receiving a single byte. Even if it wasn't a kube-probe, it's
// probably fine to not restore the application in case a connection is closed
// without receiving a single byte. If kubernetes ever starts to send something,
// this would need to be reworked but should be caught by e2e tests.
// https://github.com/kubernetes/kubernetes/blob/7cc3faf39d89d11c910db9ad19adfd931250e01c/pkg/probe/tcp/tcp.go#L50
func isTCPProbe(ctx context.Context, conn bufConn) bool {
	_, err := conn.Peek(1)
	return err == io.EOF
}

// isHTTPProbe detects an HTTP probe by constructing a [http.Request] from the
// conn buffer. If it's a valid HTTP request, we simply read the user agent
// string and assume it's a probe if it has the prefix "kube-probe/".
func isHTTPProbe(ctx context.Context, conn bufConn) bool {
	if _, err := conn.Peek(1); err != nil {
		return false
	}
	b, err := conn.Peek(min(conn.r.Buffered(), conn.r.Size()))
	if err != nil && err != io.EOF {
		log.G(ctx).WithError(err).Error("peek")
		return false
	}
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(b)))
	if err != nil {
		log.G(ctx).WithError(err).Error("req")
		return false
	}
	return strings.HasPrefix(req.Header.Get("User-Agent"), "kube-probe/")
}

// probeResponse writes an HTTP response to conn that satisfies the kubelet. It
// sets the status code to [http.StatusNoContent] as the response does not
// contain a body and it makes it simpler to detect a probe response in testing.
func probeResponse(conn net.Conn) error {
	resp := http.Response{
		ProtoMajor: 1,
		ProtoMinor: 1,
		StatusCode: http.StatusNoContent,
	}
	return resp.Write(conn)
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func newBufferedConn(c net.Conn, size int) bufConn {
	return bufConn{c, bufio.NewReaderSize(c, size)}
}

func (b bufConn) Peek(n int) ([]byte, error) {
	return b.r.Peek(n)
}

func (b bufConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}
