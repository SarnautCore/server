package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

const applicationProtocol = "sarnaut/1"

// NewDevServerTLSConfig creates an in-memory self-signed certificate for local use.
func NewDevServerTLSConfig() (*tls.Config, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate development private key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial number: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: "SarnautCore development"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	certificateDER, err := x509.CreateCertificate(
		rand.Reader,
		&template,
		&template,
		&privateKey.PublicKey,
		privateKey,
	)
	if err != nil {
		return nil, fmt.Errorf("create development certificate: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{certificateDER},
			PrivateKey:  privateKey,
		}},
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{applicationProtocol},
	}, nil
}

// NewDevClientTLSConfig trusts the ephemeral certificate created by
// NewDevServerTLSConfig. It must not be used outside local development.
func NewDevClientTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // Development certificates are ephemeral and self-signed.
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{applicationProtocol},
	}
}

// LoadPrivateServerTLSConfig loads the shard certificate for the private
// listener. TLS 1.3 is mandatory. The HMAC exchange authenticates the gateway;
// M4 replaces it with client-certificate policy.
func LoadPrivateServerTLSConfig(certificatePath, privateKeyPath string) (*tls.Config, error) {
	if certificatePath == "" || privateKeyPath == "" {
		return nil, fmt.Errorf("private TLS certificate and key paths are required")
	}
	certificate, err := tls.LoadX509KeyPair(certificatePath, privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load private TLS certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{applicationProtocol},
	}, nil
}

// LoadPrivateClientTLSConfig pins the internal CA used by shard certificates.
// It never falls back to InsecureSkipVerify.
func LoadPrivateClientTLSConfig(caPath, serverName string) (*tls.Config, error) {
	if caPath == "" || serverName == "" {
		return nil, fmt.Errorf("private CA path and server name are required")
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read private CA certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("private CA file contains no certificates")
	}
	return &tls.Config{
		RootCAs: roots, ServerName: serverName,
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{applicationProtocol},
	}, nil
}
