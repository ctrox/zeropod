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

	caCertPool := x509.NewCertPool()
	ca, serverCert, clientCert := prepareTLS(t)
	caCertPool.AddCert(ca)
	tlsConfig := serverTLSConfig(caCertPool, serverCert)
	psp := newPageServerProxy("127.0.0.1:0", socket, tlsConfig, nil, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	if err := psp.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("started server on port %d", psp.Port())

	dial := tls.Dialer{
		Config: &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			RootCAs:      caCertPool,
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

func prepareTLS(t *testing.T) (*x509.Certificate, tls.Certificate, tls.Certificate) {
	caCert, err := GenCert(nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	ca, err := x509.ParseCertificate(caCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	serverCert, err := GenCert(ca, caCert.PrivateKey.(ed25519.PrivateKey), net.ParseIP("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}

	clientCert, err := GenCert(ca, caCert.PrivateKey.(ed25519.PrivateKey), net.ParseIP("127.0.0.2"))
	if err != nil {
		t.Fatal(err)
	}

	return ca, serverCert, clientCert
}
