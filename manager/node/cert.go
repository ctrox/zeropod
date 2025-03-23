package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
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
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(time.Hour * 87600),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth,
		},
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

	caCertPool := x509.NewCertPool()
	caCertPool.AddCert(ca)
	return serverTLSConfig(caCertPool, cert), nil
}

func serverTLSConfig(ca *x509.CertPool, cert tls.Certificate) *tls.Config {
	return &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca,
		Certificates: []tls.Certificate{cert},
		RootCAs:      ca,
		MinVersion:   tls.VersionTLS13,
	}
}
