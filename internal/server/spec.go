package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/skpjr001/fluid-config/internal/config"
)

var (
	nameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	versionRe  = regexp.MustCompile(`^v[0-9]+[a-z0-9.-]{0,15}$`)
	allMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	fieldTypes = []string{"string", "number", "integer", "boolean", "object", "array", "any"}
)

// FieldSpec validates one top-level field of every record.
type FieldSpec struct {
	Type        string   `json:"type"` // string|number|integer|boolean|object|array|any
	Required    bool     `json:"required,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []any    `json:"enum,omitempty"`
	Min         *float64 `json:"min,omitempty"` // number value, or string length
	Max         *float64 `json:"max,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
	Format      string   `json:"format,omitempty"` // date|date-time|email|uri

	re *regexp.Regexp
}

// CacheSpec turns on the in-memory response cache for an API. Responses to
// GET requests are kept per query until ttl_seconds pass or the data changes
// (any write or file edit), and can be purged from the console.
type CacheSpec struct {
	Enabled    bool `json:"enabled"`
	TTLSeconds int  `json:"ttl_seconds,omitempty"` // default 60
	MaxEntries int  `json:"max_entries,omitempty"` // default 500 distinct queries
}

func (s *APISpec) cacheOn() bool { return s.Cache != nil && s.Cache.Enabled }

// AuthSpec: secrets may be literal, "env:VAR_NAME" or "sha256:<hex>".
type AuthSpec struct {
	Type       string            `json:"type"`             // none|bearer|api_key|basic
	Header     string            `json:"header,omitempty"` // api_key header, default X-API-Key
	Tokens     []string          `json:"tokens,omitempty"` // bearer / api_key; "name=secret" names a token for the audit trail
	Users      map[string]string `json:"users,omitempty"`  // basic: user -> password
	ReadPublic bool              `json:"read_public,omitempty"`

	secrets []namedSecret
	users   map[string]secret
}

type namedSecret struct {
	name string
	secret
}

// APISpec is one JSON file = one API: its contract, its access rules and its data.
type APISpec struct {
	Schema       string                `json:"$schema,omitempty"`
	Name         string                `json:"name"`
	Version      string                `json:"version"`
	Description  string                `json:"description,omitempty"`
	Enabled      *bool                 `json:"enabled,omitempty"`
	IDField      string                `json:"id_field,omitempty"`
	Methods      []string              `json:"methods,omitempty"`
	Auth         AuthSpec              `json:"auth"`
	AllowedIPs   []string              `json:"allowed_ips,omitempty"` // who may call it; omitted = loopback + private networks, ["*"] = anyone
	CIDRs        []string              `json:"cidrs,omitempty"`       // same as allowed_ips (kept from the first release); both are combined
	RateLimit    *config.RateLimit     `json:"rate_limit,omitempty"`  // token bucket per client IP
	RateLimitMin int                   `json:"rate_limit_per_min,omitempty"`
	CacheSeconds int                   `json:"cache_seconds,omitempty"` // HTTP Cache-Control max-age for clients
	Cache        *CacheSpec            `json:"cache,omitempty"`         // in-memory response cache on the server
	CORSOrigins  []string              `json:"cors_origins,omitempty"`
	StrictFields bool                  `json:"strict_fields,omitempty"`
	MaxRecords   int                   `json:"max_records,omitempty"` // refuse creates beyond this (0 = no limit)
	Fields       map[string]*FieldSpec `json:"fields,omitempty"`
	Data         []map[string]any      `json:"data"`

	file    string // absolute path
	rel     string // path inside apis/
	modTime time.Time
	ips     IPList
	limit   config.RateLimit
	index   map[string]int
	etag    string
}

func (s *APISpec) key() string      { return s.Version + "/" + s.Name }
func (s *APISpec) enabled() bool    { return s.Enabled == nil || *s.Enabled }
func (s *APISpec) basePath() string { return "/" + s.Version + "/" + s.Name }

func (s *APISpec) allows(method string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	return slices.Contains(s.Methods, method)
}

func (s *APISpec) allowedIPs() []string {
	return append(append([]string{}, s.AllowedIPs...), s.CIDRs...)
}

// restrictedDefault: no allowed_ips/cidrs, so loopback and private networks only.
func (s *APISpec) restrictedDefault() bool { return len(s.AllowedIPs)+len(s.CIDRs) == 0 }

func (s *APISpec) allowsIP(a netip.Addr) (string, bool) { return s.ips.Match(a) }

// public reports whether anyone on the internet may call it.
func (s *APISpec) public() bool {
	_, ok := s.ips.Match(netip.MustParseAddr("8.8.8.8"))
	return ok
}

func (s *APISpec) normalize() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !nameRe.MatchString(s.Name) {
		bad("name must match %s", nameRe)
	}
	if !versionRe.MatchString(s.Version) {
		bad("version must look like v1, v2, v1-beta")
	}
	if s.IDField == "" {
		s.IDField = "id"
	}
	if len(s.Methods) == 0 {
		s.Methods = []string{"GET"}
	}
	for i, m := range s.Methods {
		m = strings.ToUpper(m)
		if !slices.Contains(allMethods, m) {
			bad("unsupported method %q (use %s)", m, strings.Join(allMethods, ", "))
		}
		s.Methods[i] = m
	}
	if s.RateLimitMin < 0 || s.CacheSeconds < 0 || s.MaxRecords < 0 {
		bad("rate_limit_per_min, cache_seconds and max_records must be >= 0")
	}
	s.limit = config.RateLimit{}
	switch {
	case s.RateLimit != nil:
		if err := s.RateLimit.Validate(); err != nil {
			bad("rate_limit: %v", err)
		}
		s.limit = *s.RateLimit
	case s.RateLimitMin > 0:
		s.limit = config.RateLimit{RequestsPerMinute: s.RateLimitMin, Burst: s.RateLimitMin}
	}
	if c := s.Cache; c != nil {
		if c.TTLSeconds < 0 || c.MaxEntries < 0 || c.TTLSeconds > 86400 || c.MaxEntries > 100000 {
			bad("cache: ttl_seconds must be 0-86400 and max_entries 0-100000")
		}
		if c.TTLSeconds == 0 {
			c.TTLSeconds = 60
		}
		if c.MaxEntries == 0 {
			c.MaxEntries = 500
		}
	}
	if err := s.Auth.normalize(); err != nil {
		bad("auth: %v", err)
	}
	ips := s.allowedIPs()
	if len(ips) == 0 {
		ips = defaultAllow
	}
	l, err := parseIPList(ips)
	if err != nil {
		bad("allowed_ips/cidrs%v", err)
	}
	s.ips = l
	for _, o := range s.CORSOrigins {
		if o != "*" && !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://") {
			bad("cors_origins: %q must be * or an origin like https://app.example.com", o)
		}
	}
	for name, f := range s.Fields {
		if f == nil {
			bad("field %q is null", name)
			continue
		}
		if f.Type == "" {
			f.Type = "any"
		}
		if !slices.Contains(fieldTypes, f.Type) {
			bad("field %q: unknown type %q (use %s)", name, f.Type, strings.Join(fieldTypes, ", "))
		}
		switch f.Format {
		case "", "date", "date-time", "email", "uri":
		default:
			bad("field %q: unknown format %q (date, date-time, email, uri)", name, f.Format)
		}
		f.re = nil
		if f.Pattern != "" {
			re, err := regexp.Compile(f.Pattern)
			if err != nil {
				bad("field %q: bad pattern: %v", name, err)
			}
			f.re = re
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if s.Data == nil {
		s.Data = []map[string]any{}
	}
	for i, row := range s.Data {
		if row == nil {
			return fmt.Errorf("data[%d]: must be an object", i)
		}
		if _, ok := row[s.IDField]; !ok {
			return fmt.Errorf("data[%d]: missing %q", i, s.IDField)
		}
		if err := s.validate(row, false); err != nil {
			return fmt.Errorf("data[%d] (%s %s): %w", i, s.IDField, toString(row[s.IDField]), err)
		}
	}
	return s.rebuild()
}

// rebuild recomputes the id index and ETag after data changes.
func (s *APISpec) rebuild() error {
	s.index = make(map[string]int, len(s.Data))
	for i, row := range s.Data {
		id := toString(row[s.IDField])
		if id == "" {
			return fmt.Errorf("data[%d]: empty %q", i, s.IDField)
		}
		if _, dup := s.index[id]; dup {
			return fmt.Errorf("duplicate %s %q", s.IDField, id)
		}
		s.index[id] = i
	}
	b, _ := json.Marshal(s.Data)
	h := sha256.Sum256(b)
	s.etag = `W/"` + hex.EncodeToString(h[:8]) + `"`
	return nil
}

// withData returns a copy of the spec holding new data (copy-on-write).
func (s *APISpec) withData(data []map[string]any) (*APISpec, error) {
	n := *s
	n.Data = data
	if err := n.rebuild(); err != nil {
		return nil, err
	}
	return &n, nil
}

// record returns one record by id.
func (s *APISpec) record(id string) (map[string]any, bool) {
	i, ok := s.index[id]
	if !ok {
		return nil, false
	}
	return s.Data[i], true
}

// recordETag is a strong validator for one record (If-Match on writes).
func recordETag(row map[string]any) string {
	b, _ := json.Marshal(row)
	h := sha256.Sum256(b)
	return `"` + hex.EncodeToString(h[:8]) + `"`
}

// encode is the file content: fixed field order, numbers as written.
func (s *APISpec) encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// decodeSpec parses an API file strictly, keeping numbers exact.
func decodeSpec(b []byte) (*APISpec, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber() // keep numbers exact (large ids, amounts)
	dec.DisallowUnknownFields()
	var spec APISpec
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("unexpected data after the JSON object")
	}
	return &spec, nil
}

// contract is the spec without its data (what the console edits).
func (s *APISpec) contract() *APISpec {
	c := s.clone()
	c.Data = nil
	return c
}

// clone deep-copies the exported part of a spec.
func (s *APISpec) clone() *APISpec {
	b, _ := json.Marshal(s)
	c, err := decodeSpec(b)
	if err != nil {
		panic(err) // a spec always round-trips
	}
	c.file, c.rel, c.modTime = s.file, s.rel, s.modTime
	return c
}

// ---------- validation ----------

func (s *APISpec) validate(row map[string]any, partial bool) error {
	if s.StrictFields {
		for k := range row {
			if _, ok := s.Fields[k]; !ok && k != s.IDField {
				return fmt.Errorf("unknown field %q", k)
			}
		}
	}
	for _, name := range sortedKeys(s.Fields) {
		f := s.Fields[name]
		v, present := row[name]
		if !present || v == nil {
			if f.Required && !partial {
				return fmt.Errorf("%q is required", name)
			}
			continue
		}
		if err := f.check(v); err != nil {
			return fmt.Errorf("%q %w", name, err)
		}
	}
	return nil
}

func (f *FieldSpec) check(v any) error {
	var num *float64
	var str *string
	switch f.Type {
	case "string":
		x, ok := v.(string)
		if !ok {
			return errors.New("must be a string")
		}
		str = &x
	case "number", "integer":
		n, ok := v.(json.Number)
		if !ok {
			return errors.New("must be a number")
		}
		x, err := n.Float64()
		if err != nil {
			return errors.New("must be a number")
		}
		if f.Type == "integer" {
			if _, err := n.Int64(); err != nil {
				return errors.New("must be an integer")
			}
		}
		num = &x
	case "boolean":
		if _, ok := v.(bool); !ok {
			return errors.New("must be a boolean")
		}
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return errors.New("must be an object")
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return errors.New("must be an array")
		}
	}
	if len(f.Enum) > 0 {
		ok := false
		for _, e := range f.Enum {
			if toString(e) == toString(v) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("must be one of %v", f.Enum)
		}
	}
	measure := func() (float64, bool) {
		if num != nil {
			return *num, true
		}
		if str != nil {
			return float64(len([]rune(*str))), true
		}
		return 0, false
	}
	if m, ok := measure(); ok {
		if f.Min != nil && m < *f.Min {
			return fmt.Errorf("must be >= %v", *f.Min)
		}
		if f.Max != nil && m > *f.Max {
			return fmt.Errorf("must be <= %v", *f.Max)
		}
	}
	if str != nil {
		if f.re != nil && !f.re.MatchString(*str) {
			return fmt.Errorf("must match %s", f.Pattern)
		}
		switch f.Format {
		case "date":
			if _, err := time.Parse("2006-01-02", *str); err != nil {
				return errors.New("must be a date YYYY-MM-DD")
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339, *str); err != nil {
				return errors.New("must be an RFC3339 date-time")
			}
		case "email":
			if at := strings.Index(*str, "@"); at < 1 || at == len(*str)-1 || strings.ContainsAny(*str, " \t\r\n") {
				return errors.New("must be an email")
			}
		case "uri":
			if !strings.Contains(*str, "://") {
				return errors.New("must be an absolute URI")
			}
		}
	}
	return nil
}

// ---------- auth ----------

type secret struct {
	plain string
	sha   []byte
}

func parseSecret(s string) (secret, error) {
	switch {
	case strings.HasPrefix(s, "env:"):
		v := os.Getenv(s[4:])
		if v == "" {
			return secret{}, fmt.Errorf("env var %s is empty", s[4:])
		}
		return secret{plain: v}, nil
	case strings.HasPrefix(s, "sha256:"):
		b, err := hex.DecodeString(s[7:])
		if err != nil || len(b) != 32 {
			return secret{}, errors.New("sha256: needs 64 hex chars")
		}
		return secret{sha: b}, nil
	case s == "":
		return secret{}, errors.New("empty secret")
	}
	return secret{plain: s}, nil
}

func (sc secret) match(got string) bool {
	if sc.sha != nil {
		h := sha256.Sum256([]byte(got))
		return subtle.ConstantTimeCompare(h[:], sc.sha) == 1
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(sc.plain)) == 1
}

var tokenNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}=`)

// splitTokenName separates "ci-bot=env:CI_TOKEN" or "ci-bot=sha256:<hex>"
// into a name and the secret. Literal tokens are never split (they may
// contain "="); they are named token-1, token-2... by position.
func splitTokenName(t string, i int) (name, value string) {
	if loc := tokenNameRe.FindStringIndex(t); loc != nil {
		if v := t[loc[1]:]; strings.HasPrefix(v, "env:") || strings.HasPrefix(v, "sha256:") {
			return t[:loc[1]-1], v
		}
	}
	return "token-" + strconv.Itoa(i+1), t
}

func (a *AuthSpec) normalize() error {
	if a.Type == "" {
		a.Type = "none"
	}
	a.secrets, a.users = nil, nil
	switch a.Type {
	case "none":
		if len(a.Tokens) > 0 || len(a.Users) > 0 {
			return errors.New(`tokens/users are set but type is "none"`)
		}
	case "bearer", "api_key":
		if a.Type == "api_key" && a.Header == "" {
			a.Header = "X-API-Key"
		}
		if len(a.Tokens) == 0 {
			return errors.New("tokens required")
		}
		seen := map[string]bool{}
		for i, t := range a.Tokens {
			name, v := splitTokenName(t, i)
			if seen[name] {
				return fmt.Errorf("duplicate token name %q", name)
			}
			seen[name] = true
			sc, err := parseSecret(v)
			if err != nil {
				return fmt.Errorf("tokens[%d]: %w", i, err)
			}
			a.secrets = append(a.secrets, namedSecret{name, sc})
		}
	case "basic":
		if len(a.Users) == 0 {
			return errors.New("users required")
		}
		a.users = map[string]secret{}
		for u, p := range a.Users {
			if u == "" || strings.Contains(u, ":") {
				return fmt.Errorf("user %q: names must be non-empty without ':'", u)
			}
			sc, err := parseSecret(p)
			if err != nil {
				return fmt.Errorf("user %s: %w", u, err)
			}
			a.users[u] = sc
		}
	default:
		return fmt.Errorf("unknown auth type %q (none, bearer, api_key, basic)", a.Type)
	}
	return nil
}

// check returns who is calling (for the logs and the audit trail) and
// whether the credentials are valid. Every token is compared in constant time.
func (a *AuthSpec) check(r *http.Request) (string, bool) {
	match := func(got string) (string, bool) {
		who, ok := "", false
		for _, sc := range a.secrets {
			if got != "" && sc.match(got) && !ok {
				who, ok = sc.name, true
			}
		}
		return who, ok
	}
	switch a.Type {
	case "none":
		return "anonymous", true
	case "bearer":
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			return "", false
		}
		return match(strings.TrimPrefix(h, "Bearer "))
	case "api_key":
		return match(r.Header.Get(a.Header))
	case "basic":
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Basic ") {
			return "", false
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
		if err != nil {
			return "", false
		}
		u, p, ok := strings.Cut(string(raw), ":")
		sc, found := a.users[u]
		if ok && found && sc.match(p) {
			return u, true
		}
	}
	return "", false
}

// plainSecrets are the credential values known in clear (for masking logs).
func (a *AuthSpec) plainSecrets() []string {
	var out []string
	for _, s := range a.secrets {
		if s.plain != "" {
			out = append(out, s.plain)
		}
	}
	for u, s := range a.users {
		if s.plain != "" {
			out = append(out, s.plain, base64.StdEncoding.EncodeToString([]byte(u+":"+s.plain)))
		}
	}
	return out
}

// ---------- value helpers ----------

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
