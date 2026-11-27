package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/skpjr001/fluid-config/internal/config"
)

// ---------- base paths ----------

// stripBasePath serves a listener under a path prefix. Requests that carry
// the prefix have it removed; requests without it (the reverse proxy already
// stripped it) pass through unchanged. So both proxy styles work.
func stripBasePath(base string, next http.Handler) http.Handler {
	if base == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == base || strings.HasPrefix(p, base+"/") {
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(p, base)
			if r2.URL.Path == "" {
				r2.URL.Path = "/"
			}
			if r.URL.RawPath != "" {
				r2.URL.RawPath = strings.TrimPrefix(r.URL.RawPath, base)
			}
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- TLS ----------

// certReloader serves a certificate from files and reloads it when the
// files change (e.g. after certbot renews), checked at most every 10s.
type certReloader struct {
	certFile, keyFile string
	mu                sync.Mutex
	cert              *tls.Certificate
	modTime           time.Time
	checked           time.Time
}

func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	cr := &certReloader{certFile: certFile, keyFile: keyFile}
	if err := cr.load(); err != nil {
		return nil, err
	}
	return cr, nil
}

func (cr *certReloader) load() error {
	c, err := tls.LoadX509KeyPair(cr.certFile, cr.keyFile)
	if err != nil {
		return err
	}
	fi, err := os.Stat(cr.certFile)
	if err != nil {
		return err
	}
	cr.cert, cr.modTime = &c, fi.ModTime()
	return nil
}

func (cr *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if time.Since(cr.checked) > 10*time.Second {
		cr.checked = time.Now()
		if fi, err := os.Stat(cr.certFile); err == nil && !fi.ModTime().Equal(cr.modTime) {
			if err := cr.load(); err != nil {
				slog.Error("certificate reload failed, keeping the current one", "kind", "system", "file", cr.certFile, "err", err)
			} else {
				slog.Info("certificate reloaded", "kind", "system", "file", cr.certFile)
			}
		}
	}
	return cr.cert, nil
}

// tlsSetup is the runtime form of a TLSConfig.
type tlsSetup struct {
	config      *tls.Config
	httpHandler http.Handler // ACME HTTP-01 + redirect, when http_challenge_addr is set
	httpAddr    string
}

// buildTLS creates the server TLS config. http1Only is needed for the
// forward proxy: CONNECT tunnels hijack the connection, which HTTP/2 cannot.
func buildTLS(t *config.TLSConfig, defaultCacheDir string, http1Only bool) (*tlsSetup, error) {
	if t == nil {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.MinVersion == "1.3" {
		cfg.MinVersion = tls.VersionTLS13
	}
	out := &tlsSetup{config: cfg}
	if t.ACME == nil {
		cr, err := newCertReloader(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading certificate: %w", err)
		}
		cfg.GetCertificate = cr.GetCertificate
	} else {
		a := t.ACME
		cache := a.CacheDir
		if cache == "" {
			cache = defaultCacheDir
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(cache),
			HostPolicy: autocert.HostWhitelist(a.Domains...),
			Email:      a.Email,
		}
		if a.DirectoryURL != "" || a.CAFile != "" {
			c := &acme.Client{DirectoryURL: a.DirectoryURL}
			if a.CAFile != "" {
				pem, err := os.ReadFile(a.CAFile)
				if err != nil {
					return nil, fmt.Errorf("acme.ca_file: %w", err)
				}
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(pem) {
					return nil, fmt.Errorf("acme.ca_file %s: no PEM certificates", a.CAFile)
				}
				c.HTTPClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
					Proxy: nil, TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
			}
			m.Client = c
		}
		cfg.GetCertificate = m.GetCertificate
		cfg.NextProtos = append(cfg.NextProtos, acme.ALPNProto) // TLS-ALPN-01
		if a.HTTPChallengeAddr != "" {
			out.httpHandler = stripDefaultHTTPPort(m.HTTPHandler(nil))
			out.httpAddr = a.HTTPChallengeAddr
		}
	}
	if http1Only {
		cfg.NextProtos = append(cfg.NextProtos, "http/1.1")
	} else {
		cfg.NextProtos = append(cfg.NextProtos, "h2", "http/1.1")
	}
	return out, nil
}

// stripDefaultHTTPPort removes ":80" from the Host header. Some ACME servers
// (Pebble, others) validate http://name:80/..., and autocert's host policy
// compares the Host header including the port, refusing the challenge.
func stripDefaultHTTPPort(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := strings.CutSuffix(r.Host, ":80"); ok {
			r.Host = h
		}
		next.ServeHTTP(w, r)
	})
}

// listen opens a listener, wrapped in TLS when configured.
func listen(addr string, ts *tlsSetup) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if ts != nil {
		return tls.NewListener(ln, ts.config), nil
	}
	return ln, nil
}
