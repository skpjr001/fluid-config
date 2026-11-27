// Package config loads and validates the static config.json.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/skpjr001/fluid-config/internal/telemetry"
)

// SchemaBase is where the published JSON Schemas live. Files written by
// fluid-config point their "$schema" here so editors can validate them.
const SchemaBase = "https://raw.githubusercontent.com/skpjr001/fluid-config/main/schema/"

// StaticConfig is read once at startup. Everything that changes at runtime
// (API files, admin tokens, access lists) lives in DynamicDir.
type StaticConfig struct {
	Schema          string           `json:"$schema,omitempty"`
	APIAddr         string           `json:"api_addr"`            // the generated REST APIs (public)
	AdminAddr       string           `json:"admin_addr"`          // console + admin API; keep on 127.0.0.1
	APIBasePath     string           `json:"api_base_path"`       // e.g. "/config" -> /config/v1/holidays
	AdminBasePath   string           `json:"admin_base_path"`     // path prefix for the console
	APITLS          *TLSConfig       `json:"api_tls,omitempty"`   // serve the APIs over HTTPS without a reverse proxy
	AdminTLS        *TLSConfig       `json:"admin_tls,omitempty"` // serve the console over HTTPS
	DynamicDir      string           `json:"dynamic_dir"`         // apis/, admin.json, access.json (hot-reloaded)
	ReloadInterval  Duration         `json:"reload_interval"`     // how often DynamicDir is checked for changes
	TrustedProxies  []string         `json:"trusted_proxies"`     // CIDRs whose X-Forwarded-For is believed
	MaxBodyBytes    int64            `json:"max_body_bytes"`      // request body cap on the API listener
	MaxPageSize     int              `json:"max_page_size"`       // largest ?limit= a list accepts
	PublicCatalog   bool             `json:"public_catalog"`      // /_catalog and /openapi.json without a token
	ShutdownTimeout Duration         `json:"shutdown_timeout"`    // graceful shutdown: time to finish in-flight requests
	ShutdownDelay   Duration         `json:"shutdown_delay"`      // keep serving this long after SIGTERM while /readyz fails
	Database        DatabaseConfig   `json:"database"`
	Telemetry       telemetry.Config `json:"telemetry"`

	Trusted []netip.Prefix `json:"-"` // parsed trusted_proxies
}

type DatabaseConfig struct {
	Path               string `json:"path"`
	RetentionDays      int    `json:"retention_days"`       // request logs older than this are pruned
	AuditRetentionDays int    `json:"audit_retention_days"` // record history older than this is pruned
	LogLevel           string `json:"log_level"`            // minimum level stored in the log table

	// Every API call stores its request/response headers and bodies.
	CapturePayloads     bool  `json:"capture_payloads"`
	CaptureMaxBodyBytes int64 `json:"capture_max_body_bytes"` // per body; longer bodies are cut (the full size is recorded)
}

// TelemetryConfig exports OpenTelemetry traces over OTLP/HTTP.
// RateLimit is a token bucket per client IP. Zero RequestsPerMinute disables it.
type RateLimit struct {
	RequestsPerMinute int `json:"requests_per_minute"`
	Burst             int `json:"burst"`
}

// Duration marshals as a Go duration string ("2s", "5m").
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`duration must be a string like "30s" or "5m"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q", s)
	}
	*d = Duration(v)
	return nil
}

func Default() *StaticConfig {
	return &StaticConfig{
		APIAddr:         ":8090",
		AdminAddr:       "127.0.0.1:9090",
		DynamicDir:      "./dynamic",
		ReloadInterval:  Duration(2 * time.Second),
		MaxBodyBytes:    1 << 20,
		MaxPageSize:     1000,
		ShutdownTimeout: Duration(15 * time.Second),
		Database: DatabaseConfig{Path: "./data/fluid-config.db", RetentionDays: 14, AuditRetentionDays: 365,
			LogLevel: "info", CapturePayloads: true, CaptureMaxBodyBytes: 1 << 20},
		Telemetry: telemetry.Config{ServiceName: "fluid-config", SampleRatio: 1},
	}
}

// decodeStrict decodes exactly one JSON value and rejects unknown fields,
// so typos in config files are errors instead of silently ignored.
func DecodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

func Load(path string) (*StaticConfig, error) {
	c := Default()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := DecodeStrict(bytes.NewReader(b), c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, hintLegacy(err))
		}
	case !os.IsNotExist(err):
		if fi, serr := os.Stat(path); serr == nil && fi.IsDir() {
			return nil, fmt.Errorf("%s is a directory, not a file: a container bind mount of a file that did not exist on the host creates a directory; remove it and create the file on the host (e.g. cp examples/config.docker.json config.json), or drop the mount and let fluid-config create its defaults", path)
		}
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// hintLegacy explains settings of the first fluid-config release.
func hintLegacy(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, `"addr"`):
		return fmt.Errorf("%w (renamed to \"api_addr\"; the console has its own \"admin_addr\")", err)
	case strings.Contains(msg, `"apis_dir"`):
		return fmt.Errorf("%w (API files now live in <dynamic_dir>/apis: set \"dynamic_dir\" and move the files)", err)
	case strings.Contains(msg, `"base_path"`):
		return fmt.Errorf("%w (renamed to \"api_base_path\")", err)
	case strings.Contains(msg, `"trust_forwarded_for"`):
		return fmt.Errorf("%w (use \"trusted_proxies\": [\"<reverse proxy CIDR>\"] instead)", err)
	case strings.Contains(msg, `"catalog_token"`):
		return fmt.Errorf("%w (/_catalog and /openapi.json now take any admin token from admin.json; set \"public_catalog\": true to open them)", err)
	}
	return err
}

func (c *StaticConfig) Validate() error {
	var errs []error
	for _, bp := range []struct {
		name string
		p    *string
	}{{"api_base_path", &c.APIBasePath}, {"admin_base_path", &c.AdminBasePath}} {
		n, err := NormalizeBasePath(*bp.p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", bp.name, err))
		}
		*bp.p = n
	}
	for name, t := range map[string]*TLSConfig{"api_tls": c.APITLS, "admin_tls": c.AdminTLS} {
		if err := t.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	if a, b := c.APITLS, c.AdminTLS; a != nil && b != nil && a.ACME != nil && b.ACME != nil &&
		a.ACME.HTTPChallengeAddr != "" && a.ACME.HTTPChallengeAddr == b.ACME.HTTPChallengeAddr {
		errs = append(errs, errors.New("api_tls and admin_tls use the same acme.http_challenge_addr; set it on one listener only"))
	}
	if c.APIAddr == "" || c.AdminAddr == "" {
		errs = append(errs, errors.New("api_addr and admin_addr are required"))
	} else if c.APIAddr == c.AdminAddr && !strings.HasSuffix(c.APIAddr, ":0") {
		errs = append(errs, errors.New("api_addr and admin_addr must differ"))
	}
	if c.DynamicDir == "" {
		errs = append(errs, errors.New("dynamic_dir is required"))
	}
	if c.ReloadInterval <= 0 {
		errs = append(errs, errors.New("reload_interval must be > 0"))
	}
	if c.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("max_body_bytes must be > 0"))
	}
	if c.MaxPageSize < 1 {
		errs = append(errs, errors.New("max_page_size must be >= 1"))
	}
	c.Trusted = nil
	for _, s := range c.TrustedProxies {
		p, err := ParsePrefix(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("trusted_proxies: %w", err))
			continue
		}
		c.Trusted = append(c.Trusted, p)
	}
	if c.Database.Path == "" {
		errs = append(errs, errors.New("database.path is required"))
	}
	if c.ShutdownTimeout.D() < time.Second || c.ShutdownTimeout.D() > 10*time.Minute {
		errs = append(errs, errors.New("shutdown_timeout must be between 1s and 10m"))
	}
	if c.ShutdownDelay.D() < 0 || c.ShutdownDelay.D() > 5*time.Minute {
		errs = append(errs, errors.New("shutdown_delay must be between 0s and 5m"))
	}
	if c.Database.CaptureMaxBodyBytes < 0 {
		errs = append(errs, errors.New("database.capture_max_body_bytes must be >= 0 (0 stores headers only)"))
	}
	if c.Database.RetentionDays < 1 || c.Database.AuditRetentionDays < 1 {
		errs = append(errs, errors.New("database.retention_days and database.audit_retention_days must be >= 1"))
	}
	if _, ok := LogLevels[strings.ToLower(c.Database.LogLevel)]; !ok {
		errs = append(errs, errors.New("database.log_level must be debug, info, warn or error"))
	}
	if c.Telemetry.SampleRatio < 0 || c.Telemetry.SampleRatio > 1 {
		errs = append(errs, errors.New("telemetry.sample_ratio must be between 0 and 1"))
	}
	if c.Telemetry.ServiceName == "" {
		errs = append(errs, errors.New("telemetry.service_name must not be empty"))
	}
	return errors.Join(errs...)
}

func (r RateLimit) Validate() error {
	if r.RequestsPerMinute < 0 || r.Burst < 0 {
		return errors.New("values must be >= 0")
	}
	if r.RequestsPerMinute > 0 && r.Burst == 0 {
		return errors.New("burst must be > 0 when requests_per_minute is set")
	}
	return nil
}

// parsePrefix accepts "10.0.0.0/8" or a bare IP ("1.2.3.4" -> /32).
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("bad IP or CIDR %q", s)
		}
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("bad CIDR %q", s)
	}
	if p.Addr().Is4In6() {
		p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p.Masked(), nil
}

func PrefixesContain(ps []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// LogLevels maps database.log_level names to slog levels.
var LogLevels = map[string]slog.Level{
	"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
}

var basePathRe = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// normalizeBasePath turns "", "/" into "" and "/a/b/" into "/a/b".
// NormalizeBasePath trims and checks a base path such as "/config".
func NormalizeBasePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", nil
	}
	if !basePathRe.MatchString(p) {
		return "", fmt.Errorf("base path %q must look like /segment[/segment] (letters, digits, . _ ~ -)", p)
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("base path %q must not contain . or .. segments", p)
		}
	}
	return p, nil
}
