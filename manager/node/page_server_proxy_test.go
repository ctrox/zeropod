package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestPageServerProxy(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "psp.sock")

	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	ca, cert := prepareTLS(t)
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
	psp := newPageServerProxy("localhost:0", socket, tlsConfig, nil, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	if err := psp.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("started server on port %d", psp.Port())

	caCertPool := x509.NewCertPool()
	caCertPool.AddCert(ca)
	dial := tls.Dialer{
		Config: &tls.Config{
			RootCAs: caCertPool,
		},
	}

	conn, err := dial.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", psp.Port()))
	if err != nil {
		t.Fatal(err)
	}

	sockConn, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(sockConn, sockConn)

	testData := []byte("fooo")
	conn.Write(testData)
	buf := make([]byte, len(testData))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal("error reading from proxy connection")
	}
	t.Logf("read %d bytes", n)

	if !bytes.Equal(buf, testData) {
		t.Fatalf("read data %q does not equal test data %q", buf, testData)
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sockConn.Close(); err != nil {
		t.Fatal(err)
	}

	if err := psp.Wait(); err != nil {
		t.Fatal(err)
	}
}

func prepareTLS(t *testing.T) (*x509.Certificate, tls.Certificate) {
	caCert, err := GenCert(nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ca, err := x509.ParseCertificate(caCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	cert, err := GenCert(ca, caCert.PrivateKey.(ed25519.PrivateKey), net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}

	return ca, cert
}
