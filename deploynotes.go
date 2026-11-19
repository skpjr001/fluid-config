package main

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DeployNote is one piece of deployment advice, logged at startup with the
// matching level and shown in `fluid-config check` and the console.
type DeployNote struct {
	Level   string `json:"level"` // info | warn | error
	Topic   string `json:"topic"` // runtime, api, admin, access, tls, storage, config
	Message string `json:"message"`
}

// hostEnv describes where the agent runs; detectEnv fills it, tests fake it.
type hostEnv struct {
	Container string   // "", docker, podman, kubernetes, container
	Systemd   bool     // started by systemd
	EUID      int      // effective user id
	Mounts    []string // mount points (from /proc/self/mountinfo)
}

func detectEnv() hostEnv {
	e := hostEnv{EUID: os.Geteuid(), Systemd: os.Getenv("INVOCATION_ID") != ""}
	switch {
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "":
		e.Container = "kubernetes"
	case fileExists("/run/.containerenv"):
		e.Container = "podman"
	case fileExists("/.dockerenv"):
		e.Container = "docker"
	case os.Getenv("container") != "":
		e.Container = os.Getenv("container")
	}
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if fs := strings.Fields(sc.Text()); len(fs) > 4 {
				e.Mounts = append(e.Mounts, fs[4])
			}
		}
		f.Close()
	}
	return e
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// onMount reports whether path lives on a mount other than the root filesystem.
func (e hostEnv) onMount(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, m := range e.Mounts {
		if m != "/" && (abs == m || strings.HasPrefix(abs, strings.TrimSuffix(m, "/")+"/")) {
			return true
		}
	}
	return false
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

type notes []DeployNote

func (n *notes) add(level, topic, format string, a ...any) {
	*n = append(*n, DeployNote{Level: level, Topic: topic, Message: fmt.Sprintf(format, a...)})
}

// listenScope classifies a listen address.
func listenScope(addr string) (host string, port int, loopback bool) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr, 0, false
	}
	port, _ = strconv.Atoi(p)
	if h == "localhost" {
		return h, port, true
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return h, port, a.IsLoopback()
	}
	return h, port, false
}

// deploymentNotes inspects the configuration and environment and returns
// every caveat that applies to this way of running fluid-config.
func deploymentNotes(cfg *StaticConfig, snap *snapshot, env hostEnv, envAdmin bool, now time.Time) []DeployNote {
	var n notes

	// ---- runtime ----
	switch {
	case env.Container != "":
		n.add("info", "runtime", "running in a %s container", env.Container)
		if !env.onMount(cfg.DynamicDir) {
			n.add("warn", "config", "dynamic config dir %s is not on a mounted volume: API files, and every record written through the API or console, are lost when the container is removed; mount a volume or host directory at %s", cfg.DynamicDir, cfg.DynamicDir)
		}
		if !env.onMount(filepath.Dir(cfg.Database.Path)) {
			n.add("warn", "storage", "database %s is not on a mounted volume: request logs and the change history are lost when the container is removed; mount a volume at %s",
				cfg.Database.Path, filepath.Dir(cfg.Database.Path))
		}
	case env.Systemd:
		n.add("info", "runtime", "running on the host under systemd")
	default:
		n.add("info", "runtime", "running on the host (not under systemd or a container)")
	}
	if env.Container == "" && env.EUID == 0 {
		n.add("warn", "runtime", "running as root: use a dedicated user (see deploy/systemd/fluid-config.service)")
	}
	apisDir := filepath.Join(cfg.DynamicDir, apisSubdir)
	if fileExists(apisDir) && !dirWritable(apisDir) {
		n.add("warn", "config", "%s is read-only: writes through the API and console fail with 409; only hand edits of the files change data", apisDir)
	}

	// ---- listeners ----
	for _, l := range []struct {
		name, addr, base string
		tls              *TLSConfig
	}{
		{"api", cfg.APIAddr, cfg.APIBasePath, cfg.APITLS},
		{"admin", cfg.AdminAddr, cfg.AdminBasePath, cfg.AdminTLS},
	} {
		scheme := "http"
		if l.tls != nil {
			scheme = "https"
		}
		_, port, loop := listenScope(l.addr)
		scope := "all interfaces"
		if loop {
			scope = "loopback only"
		}
		base := l.base
		if base == "" {
			base = "/"
		}
		n.add("info", l.name, "%s listener: %s://%s (%s), path %s", l.name, scheme, l.addr, scope, base)
		if port > 0 && port < 1024 && env.EUID != 0 && env.Container == "" {
			n.add("info", l.name, "port %d is privileged: the process needs CAP_NET_BIND_SERVICE (see deploy/systemd) or a port above 1024", port)
		}
		if l.tls != nil {
			n.tlsNotes(l.name, l.tls, port, now)
		}
	}

	// ---- api ----
	_, _, apiLoop := listenScope(cfg.APIAddr)
	switch {
	case cfg.APITLS != nil:
		n.add("info", "api", "APIs are served over HTTPS directly (no reverse proxy needed)")
	case len(cfg.TrustedProxies) > 0:
		n.add("info", "api", "expecting TLS to be terminated by a reverse proxy at %s; X-Forwarded-For is believed only from those addresses", strings.Join(cfg.TrustedProxies, ", "))
	case !apiLoop:
		n.add("warn", "api", "APIs are plain HTTP and no reverse proxy is trusted: API tokens travel in clear text; set api_tls, or put a TLS reverse proxy in front and list it in trusted_proxies")
	}
	for _, p := range cfg.trusted {
		if (p.Addr().Is4() && p.Bits() < 8) || (p.Addr().Is6() && p.Bits() < 16) {
			n.add("warn", "api", "trusted_proxies contains %s: almost any client can spoof its address with X-Forwarded-For; list only your reverse proxies", p)
		}
	}
	if cfg.APIBasePath != "" {
		n.add("info", "api", "APIs at %s/<version>/<name> (requests work with or without the prefix)", cfg.APIBasePath)
	}
	if len(snap.apis) == 0 {
		n.add("warn", "api", "no API files in %s: every request gets 404; create one in the console or drop a JSON file there", apisDir)
	}
	if cfg.PublicCatalog {
		n.add("warn", "api", "public_catalog is on: /_catalog and /openapi.json describe every API (paths, fields, auth types) to any allowed address without a token")
	}
	n = append(n, accessNotes(cfg, snap, env)...)

	// ---- admin ----
	_, _, adminLoop := listenScope(cfg.AdminAddr)
	switch {
	case cfg.AdminTLS != nil:
	case !adminLoop && env.Container == "":
		n.add("warn", "admin", "console on %s is plain HTTP on all interfaces: tokens travel in clear text; bind 127.0.0.1 (SSH tunnel), put a TLS reverse proxy in front, or set admin_tls", cfg.AdminAddr)
	case !adminLoop:
		n.add("info", "admin", "console listens on all interfaces inside the container: publish it as 127.0.0.1:<port> only, or behind a TLS reverse proxy")
	}
	switch {
	case cfg.AdminBasePath != "":
		n.add("info", "admin", "console at %s/ (works with or without prefix stripping)", cfg.AdminBasePath)
	case len(cfg.TrustedProxies) > 0:
		n.add("info", "admin", "console base path follows X-Forwarded-Prefix from trusted proxies; set admin_base_path to fix it")
	}
	editors, viewers := 0, 0
	for _, t := range snap.admin.Tokens {
		if t.Role == RoleEditor {
			editors++
		} else {
			viewers++
		}
	}
	if envAdmin {
		editors++
	}
	switch {
	case editors+viewers == 0:
		n.add("warn", "admin", "no admin tokens: add <dynamic_dir>/admin.json or set %s to use the console", envAdminTokenVar)
	case editors == 0:
		n.add("info", "admin", "only viewer tokens: APIs and records can be changed only through the APIs or by editing files")
	}

	// ---- shutdown ----
	if env.Container != "" {
		n.add("info", "runtime", "graceful shutdown takes up to %s (shutdown_timeout): the container stop timeout must be longer (Docker's default is 10s; set stop_grace_period: 30s, docker stop -t 30, or terminationGracePeriodSeconds in Kubernetes)", cfg.ShutdownTimeout.D())
	} else if env.Systemd {
		n.add("info", "runtime", "graceful shutdown takes up to %s (shutdown_timeout): keep TimeoutStopSec longer (the provided unit uses 30s)", cfg.ShutdownTimeout.D())
	}

	// ---- storage ----
	if db := cfg.Database; db.CapturePayloads {
		if db.CaptureMaxBodyBytes > 0 {
			n.add("info", "storage", "every API call keeps its full request and response (headers and bodies up to %s each) for %d days", humanBytes(db.CaptureMaxBodyBytes), db.RetentionDays)
		} else {
			n.add("info", "storage", "every API call keeps its request and response headers (capture_max_body_bytes is 0: no bodies) for %d days", db.RetentionDays)
		}
		n.add("warn", "storage", "captured headers and bodies are stored unmasked, including credentials (Authorization, API keys): editor tokens see them in full, viewer tokens see them masked; protect the database file")
	} else {
		n.add("info", "storage", "capture_payloads is false: logs keep call metadata only, no headers or bodies")
	}
	n.add("info", "storage", "the change history (who changed which record, before and after) is kept for %d days", cfg.Database.AuditRetentionDays)
	cached := 0
	for _, s := range snap.apis {
		if s.cacheOn() {
			cached++
		}
	}
	if cached > 0 {
		n.add("info", "api", "%d API(s) cache responses in memory; entries expire by ttl_seconds and are dropped on every change, or purge them in the console", cached)
	}
	if strings.EqualFold(cfg.Database.LogLevel, "debug") {
		n.add("info", "storage", "log_level is debug: the log table will grow quickly")
	}
	return n
}

// tlsNotes describes a listener's certificate setup.
func (n *notes) tlsNotes(name string, t *TLSConfig, port int, now time.Time) {
	if a := t.ACME; a != nil {
		dir := a.DirectoryURL
		if dir == "" {
			dir = "Let's Encrypt production"
			n.add("info", "tls", "%s: ACME uses Let's Encrypt production (rate limits apply); use the staging directory_url while testing", name)
		}
		n.add("info", "tls", "%s: certificates for %s from %s, obtained on the first HTTPS request", name, strings.Join(a.Domains, ", "), dir)
		switch {
		case a.HTTPChallengeAddr != "":
			n.add("info", "tls", "%s: HTTP-01 challenges on %s, which must be reachable from the internet as port 80; it also redirects plain HTTP to HTTPS", name, a.HTTPChallengeAddr)
		case port != 443:
			n.add("warn", "tls", "%s: listener is on port %d and http_challenge_addr is not set: TLS-ALPN-01 needs port 443, so certificates cannot be issued unless 443 is forwarded here", name, port)
		default:
			n.add("info", "tls", "%s: TLS-ALPN-01 challenges on port 443 (must be reachable from the internet)", name)
		}
		return
	}
	b, err := os.ReadFile(t.CertFile)
	if err != nil {
		n.add("error", "tls", "%s: cannot read %s: %v", name, t.CertFile, err)
		return
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		n.add("error", "tls", "%s: %s is not PEM", name, t.CertFile)
		return
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		n.add("error", "tls", "%s: %s: %v", name, t.CertFile, err)
		return
	}
	names := append(append([]string{}, c.DNSNames...), ipStrings(c.IPAddresses)...)
	left := c.NotAfter.Sub(now)
	n.add("info", "tls", "%s: certificate %s for %s, expires %s (reloaded automatically when the file changes)",
		name, t.CertFile, strings.Join(names, ", "), c.NotAfter.UTC().Format("2006-01-02"))
	switch {
	case left <= 0:
		n.add("error", "tls", "%s: certificate %s EXPIRED on %s", name, t.CertFile, c.NotAfter.UTC().Format("2006-01-02"))
	case left < 21*24*time.Hour:
		n.add("warn", "tls", "%s: certificate %s expires in %d days", name, t.CertFile, int(left.Hours()/24))
	}
	if c.IsCA && len(c.DNSNames) == 0 && len(c.IPAddresses) == 0 {
		n.add("warn", "tls", "%s: certificate %s has no subject alternative names; clients will reject it", name, t.CertFile)
	}
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

// logDeploymentNotes writes each note with its level (kind=deploy, so they
// are stored and searchable with the other logs).
func logDeploymentNotes(ns []DeployNote) {
	for _, d := range ns {
		level := slog.LevelInfo
		switch d.Level {
		case "warn":
			level = slog.LevelWarn
		case "error":
			level = slog.LevelError
		}
		slog.Log(context.Background(), level, d.Message, "kind", "deploy", "topic", d.Topic)
	}
}

// accessNotes explains who may use the console and the catalog
// (access.json) and who may call each API (allowed_ips). New ones are logged
// whenever the files change.
func accessNotes(cfg *StaticConfig, snap *snapshot, env hostEnv) []DeployNote {
	var n notes
	ac := snap.access
	if ac == nil {
		ac = emptyAccess()
	}
	public := netip.MustParseAddr("8.8.8.8")
	restricted := false
	what := map[string]string{"admin": "the console", "catalog": "/_catalog and /openapi.json"}
	for _, name := range []string{"admin", "catalog"} {
		l := &ac.Admin
		if name == "catalog" {
			l = &ac.Catalog
		}
		list := strings.Join(l.Allow, ", ")
		_, open := l.list.Match(public)
		switch {
		case len(l.Allow) == 0:
			n.add("error", "access", "%s: access.json %s.allow is empty, so every client is refused (403); add addresses, wildcards (10.0.*.*) or CIDRs", what[name], name)
			restricted = true
		case slices.Contains(l.Allow, "*"):
			if name == "admin" {
				n.add("warn", "access", "the console accepts connections from any address (admin.allow has \"*\"): only the token protects it; list your office/VPN addresses instead")
			} else {
				n.add("info", "access", "/_catalog and /openapi.json accept any address (catalog.allow has \"*\")")
			}
		case slices.Equal(l.Allow, defaultAllow):
			restricted = true
			n.add("info", "access", "%s: only loopback and private networks may connect (the restricted default: %s)", what[name], list)
		default:
			restricted = true
			level := "info"
			if open {
				level = "warn"
			}
			n.add(level, "access", "%s: only %s may connect", what[name], list)
		}
	}
	if h, _, _ := net.SplitHostPort(cfg.AdminAddr); h == "127.0.0.1" || h == "::1" || h == "localhost" {
		_, v4 := ac.Admin.list.Match(netip.MustParseAddr("127.0.0.1"))
		_, v6 := ac.Admin.list.Match(netip.MustParseAddr("::1"))
		if !v4 && !v6 {
			n.add("error", "access", "the console listens on %s only, but admin.allow does not include 127.0.0.1 or ::1: nobody can reach it; fix access.json", cfg.AdminAddr)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(snap.apis)) {
		s := snap.apis[k]
		if !s.enabled() {
			continue
		}
		writes := slices.ContainsFunc(s.Methods, func(m string) bool { return m != "GET" })
		_, open := s.ips.Match(public)
		switch {
		case s.restrictedDefault():
			restricted = true
			n.add("warn", "access", "api %s accepts only loopback and private-network callers (the restricted default): internet clients get 403; set allowed_ips in its file (e.g. [\"52.66.*.*\", \"13.232.0.0/16\"], or [\"*\"] with auth)", k)
		case open && s.Auth.Type == "none" && writes:
			n.add("error", "access", "api %s accepts writes (%s) from any address without auth: anyone can change its data; add auth or narrow allowed_ips", k, strings.Join(s.Methods, ", "))
		case open && s.Auth.Type == "none":
			n.add("info", "access", "api %s is public and read-only (no auth)", k)
		case open && s.Auth.ReadPublic:
			n.add("info", "access", "api %s: anyone may read it; writes need its %s credentials", k, s.Auth.Type)
		case open:
			n.add("info", "access", "api %s accepts any address; requests must pass its %s auth", k, s.Auth.Type)
		default:
			restricted = true
			n.add("info", "access", "api %s accepts only: %s", k, strings.Join(s.allowedIPs(), ", "))
		}
	}
	if restricted && env.Container != "" {
		n.add("warn", "access", "allowlists see the address that reaches the container: with rootless Podman or Docker's userland proxy, published ports can show the gateway instead of the real client; check source_ip in the logs, and prefer a reverse proxy listed in trusted_proxies")
	}
	if restricted && len(cfg.TrustedProxies) == 0 {
		n.add("info", "access", "allowlists are checked against the connecting address; behind a reverse proxy, list it in trusted_proxies so the real client address is checked")
	}
	return n
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
