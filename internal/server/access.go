package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/skpjr001/fluid-config/internal/config"
)

// ---------- IP rules with wildcards ----------

// ipRule matches a client address. Accepted forms:
//
//	"*"                    any address
//	"203.0.113.7"          one address (IPv4 or IPv6)
//	"10.0.0.0/8"           CIDR (IPv4 or IPv6)
//	"10.0.*.*", "10.*"     IPv4 with "*" octets (a trailing "*" covers the rest)
//	"192.168.1-5.*"        IPv4 with octet ranges
type ipRule struct {
	raw    string
	any    bool
	prefix netip.Prefix
	octets [4][2]uint8 // [lo, hi] per octet, for wildcard patterns
	isPat  bool
}

func parseIPRule(s string) (ipRule, error) {
	s = strings.TrimSpace(s)
	r := ipRule{raw: s}
	switch {
	case s == "":
		return r, errors.New("empty rule")
	case s == "*":
		r.any = true
		return r, nil
	case strings.ContainsAny(s, "*-") && !strings.Contains(s, ":"):
		return parseIPv4Pattern(s)
	}
	p, err := config.ParsePrefix(s)
	if err != nil {
		if strings.Contains(s, ":") && strings.Contains(s, "*") {
			return r, fmt.Errorf("%q: wildcards are IPv4-only; use an IPv6 CIDR such as 2001:db8::/32", s)
		}
		return r, fmt.Errorf("%q is not an IP, CIDR or IPv4 wildcard (10.0.*.*, 192.168.1-5.*)", s)
	}
	r.prefix = p
	return r, nil
}

func parseIPv4Pattern(s string) (ipRule, error) {
	r := ipRule{raw: s, isPat: true}
	parts := strings.Split(s, ".")
	if len(parts) < 4 {
		if parts[len(parts)-1] != "*" {
			return r, fmt.Errorf("%q: give four octets, or end with * (10.*, 10.0.*)", s)
		}
		for len(parts) < 4 {
			parts = append(parts, "*")
		}
	}
	if len(parts) != 4 {
		return r, fmt.Errorf("%q: an IPv4 pattern has four octets", s)
	}
	octet := func(v string) (uint8, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 255 || (len(v) > 1 && v[0] == '0') {
			return 0, fmt.Errorf("%q: %q is not an octet (0-255)", s, v)
		}
		return uint8(n), nil
	}
	for i, p := range parts {
		switch {
		case p == "*":
			r.octets[i] = [2]uint8{0, 255}
		case strings.Contains(p, "-"):
			a, b, _ := strings.Cut(p, "-")
			lo, err := octet(a)
			if err != nil {
				return r, err
			}
			hi, err := octet(b)
			if err != nil {
				return r, err
			}
			if lo > hi {
				return r, fmt.Errorf("%q: range %s is reversed", s, p)
			}
			r.octets[i] = [2]uint8{lo, hi}
		default:
			v, err := octet(p)
			if err != nil {
				return r, err
			}
			r.octets[i] = [2]uint8{v, v}
		}
	}
	return r, nil
}

func (r ipRule) match(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	switch {
	case r.any:
		return true
	case r.isPat:
		if !a.Is4() {
			return false
		}
		b := a.As4()
		for i := range 4 {
			if b[i] < r.octets[i][0] || b[i] > r.octets[i][1] {
				return false
			}
		}
		return true
	default:
		return r.prefix.Contains(a)
	}
}

// IPList is a parsed list of rules.
type IPList struct{ rules []ipRule }

func parseIPList(list []string) (IPList, error) {
	var l IPList
	var errs []error
	for i, s := range list {
		r, err := parseIPRule(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("[%d]: %w", i, err))
			continue
		}
		l.rules = append(l.rules, r)
	}
	return l, errors.Join(errs...)
}

func (l IPList) Empty() bool { return len(l.rules) == 0 }

// Match returns the first rule that matches a.
func (l IPList) Match(a netip.Addr) (string, bool) {
	for _, r := range l.rules {
		if r.match(a) {
			return r.raw, true
		}
	}
	return "", false
}

// ---------- dynamic/access.json ----------

// defaultAllow applies to an API file without allowed_ips/cidrs, and to a
// list omitted from access.json: loopback and private networks only, so
// nothing is reachable from the internet until you say so.
var defaultAllow = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

// AccessConfig restricts which client addresses may use the console and the
// API catalog. Each API file carries its own allowed_ips instead.
// An omitted list means defaultAllow, [] means nobody, ["*"] means everyone.
type AccessConfig struct {
	Schema  string     `json:"$schema,omitempty"`
	Admin   AccessList `json:"admin"`   // console, admin API, /metrics (file-only: never changed over the API)
	Catalog AccessList `json:"catalog"` // /_catalog and /openapi.json on the API listener
}

type AccessList struct {
	Allow []string `json:"allow"` // omitted = loopback + private networks, [] = nobody, ["*"] = everyone

	list IPList
}

func (a *AccessConfig) normalize() error {
	var errs []error
	for _, s := range []struct {
		name string
		l    *AccessList
	}{{"admin", &a.Admin}, {"catalog", &a.Catalog}} {
		if s.l.Allow == nil {
			s.l.Allow = append([]string(nil), defaultAllow...)
		}
		l, err := parseIPList(s.l.Allow)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.allow%w", s.name, err))
		}
		s.l.list = l
	}
	return errors.Join(errs...)
}

// defaultAccessFile is what first start writes to access.json.
func defaultAccessFile() *AccessConfig {
	a := emptyAccess()
	a.Schema = config.SchemaBase + "access.schema.json"
	return a
}

// emptyAccess is the configuration when access.json does not exist.
func emptyAccess() *AccessConfig {
	a := &AccessConfig{}
	a.normalize()
	return a
}

// ---------- enforcement ----------

// isHealthPath lets local health checks (Docker HEALTHCHECK, fluid-config
// healthcheck, kubelet via localhost) through even when an allowlist does
// not include loopback.
func isHealthPath(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	return strings.HasSuffix(p, "/livez") || strings.HasSuffix(p, "/readyz") || strings.HasSuffix(p, "/healthz")
}

// accessGuard refuses console clients outside admin.allow with 403 (an empty
// list refuses everyone), before authentication. The address is the one
// ClientIP resolves (X-Forwarded-For only from trusted_proxies).
func (app *App) accessGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := ClientIP(r, app.cfg.Trusted)
		if _, ok := app.store.Access().Admin.list.Match(ip); ok {
			next.ServeHTTP(w, r)
			return
		}
		if isHealthPath(r) && peerAddr(r).IsLoopback() {
			next.ServeHTTP(w, r)
			return
		}
		app.metrics.Inc("fluid_access_denied_total", "listener", "admin")
		http.Error(w, "forbidden: your address is not allowed", http.StatusForbidden)
		slog.Warn("access denied: source IP not in admin allowlist", "kind", "access", "direction", "inbound",
			"route", "admin", "source_ip", ip.String(), "method", r.Method, "url", r.URL.Path, "status", http.StatusForbidden,
			"capture", app.quickCapture(r, http.StatusForbidden, w.Header(), "forbidden: your address is not allowed\n"))
	})
}

func peerAddr(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}
