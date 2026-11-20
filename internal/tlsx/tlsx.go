// Package tlsx builds the TLS setup for each SSL mode.
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/sahand363/blendartai/internal/config"
)

// Setup is what the server needs to serve HTTPS. Extra is an optional
// second listener (port 80 for Let's Encrypt HTTP-01) and may be nil.
type Setup struct {
	TLSConfig *tls.Config
	Extra     *http.Server
	Note      string
}

// Build returns nil for plain-HTTP modes.
func Build(env config.Env, logger *slog.Logger) (*Setup, error) {
	switch env.SSLMode {
	case config.SSLSelfSigned:
		crt, key, err := ensureSelfSigned(filepath.Join(env.DataDir, "tls"), env.Domain)
		if err != nil {
			return nil, err
		}
		r, err := newReloader(crt, key)
		if err != nil {
			return nil, err
		}
		return &Setup{TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.get}, Note: "self-signed certificate"}, nil

	case config.SSLFiles:
		r, err := newReloader(env.CertFile, env.KeyFile)
		if err != nil {
			return nil, err
		}
		return &Setup{TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.get}, Note: "certificate from " + env.CertFile}, nil

	case config.SSLLetsEncrypt:
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(env.Domain),
			Cache:      autocert.DirCache(filepath.Join(env.DataDir, "acme")),
			Email:      env.Email,
		}
		cfg := m.TLSConfig()
		cfg.MinVersion = tls.VersionTLS12
		s := &Setup{TLSConfig: cfg}
		if env.Port == 443 {
			s.Note = "Let's Encrypt via TLS-ALPN on 443"
			return s, nil
		}
		// The panel is on another port, so Let's Encrypt must reach us on
		// port 80. Only use it when it is free: never take it from nginx,
		// x-ui or anything else.
		if !PortFree(80) {
			s.Note = "Let's Encrypt: port 80 is busy and the panel is not on 443, so new certificates cannot be issued (a cached one still works)"
			logger.Warn(s.Note)
			return s, nil
		}
		s.Extra = &http.Server{Addr: ":80", Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 10 * time.Second}
		s.Note = "Let's Encrypt via HTTP-01 on port 80"
		return s, nil
	}
	return nil, nil
}

// PortFree reports whether a TCP port can be bound on all interfaces.
func PortFree(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// reloader re-reads the certificate when the files change (certbot renewals).
type reloader struct {
	crt, key string
	mu       sync.Mutex
	cert     *tls.Certificate
	mod      time.Time
	checked  time.Time
}

func newReloader(crt, key string) (*reloader, error) {
	r := &reloader{crt: crt, key: key}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *reloader) load() error {
	fi, err := os.Stat(r.crt)
	if err != nil {
		return err
	}
	c, err := tls.LoadX509KeyPair(r.crt, r.key)
	if err != nil {
		return err
	}
	r.cert, r.mod = &c, fi.ModTime()
	return nil
}

func (r *reloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) > time.Minute {
		r.checked = time.Now()
		if fi, err := os.Stat(r.crt); err == nil && !fi.ModTime().Equal(r.mod) {
			_ = r.load()
		}
	}
	if r.cert == nil {
		return nil, errors.New("no certificate")
	}
	return r.cert, nil
}

func ensureSelfSigned(dir, host string) (string, string, error) {
	crt, key := filepath.Join(dir, "self.crt"), filepath.Join(dir, "self.key")
	if _, err := os.Stat(crt); err == nil {
		if _, err := os.Stat(key); err == nil {
			return crt, key, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "blendartai"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	if host != "" {
		if ip := net.ParseIP(host); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &pk.PublicKey, pk)
	if err != nil {
		return "", "", err
	}
	kb, err := x509.MarshalECPrivateKey(pk)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", err
	}
	return crt, key, nil
}
