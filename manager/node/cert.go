package node

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

func GenCert(caCert *x509.Certificate, caKey ed25519.PrivateKey, ipAddresses ...net.IP) (tls.Certificate, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"ctrox.dev"},
			Country:      []string{"CH"},
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(time.Hour * 87600),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if len(ipAddresses) > 0 {
		template.IPAddresses = ipAddresses
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generating key: %w", err)
	}

	if caCert == nil {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign
		template.BasicConstraintsValid = true
		caCert = &template
	}
	if caKey == nil {
		caKey = priv
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, caCert, pub, caKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("creating certificate: %w", err)
	}

	return tls.Certificate{Certificate: [][]byte{derBytes}, PrivateKey: priv}, nil
}

func initTLS(host string) (*tls.Config, error) {
	caCert, err := tls.LoadX509KeyPair(caCertFile, caKeyFile)
	if err != nil {
		return nil, err
	}
	ca, err := x509.ParseCertificate(caCert.Certificate[0])
	if err != nil {
		return nil, err
	}

	cert, err := GenCert(ca, caCert.PrivateKey.(ed25519.PrivateKey), net.ParseIP(host))
	if err != nil {
		return nil, err
	}
	if err := writeCert(cert.Certificate[0], cert.PrivateKey, tlsKeyFile, tlsCertFile); err != nil {
		return nil, err
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AddCert(ca)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
		// TODO: TLS13 is currently broken. The reason for this is unclear but
		// CRIU fails with: Error (criu/page-xfer.c:1635): page-xfer: BUG
		// just after gnutls claims the handshake has been completed. Sometimes
		// it also works so it's just flaky.
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12,
	}, nil
}

func writeCert(derBytes []byte, key crypto.PrivateKey, certName, keyName string) error {
	certOut, err := os.Create(certName)
	if err != nil {
		return fmt.Errorf("creating cert file: %w", err)
	}

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("writing cert data: %w", err)
	}

	if err := certOut.Close(); err != nil {
		return fmt.Errorf("error closing cert: %w", err)
	}

	keyOut, err := os.OpenFile(keyName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("creating key file: %w", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("unable to marshal private key: %w", err)
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		return fmt.Errorf("writing key data: %w", err)
	}

	if err := keyOut.Close(); err != nil {
		return fmt.Errorf("closing key file: %w", err)
	}
	return nil
}
