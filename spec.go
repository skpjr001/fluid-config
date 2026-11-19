package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var (
	nameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	versionRe  = regexp.MustCompile(`^v[0-9]+[a-z0-9.-]{0,15}$`)
	allMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
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
	Format      string   `json:"format,omitempty"` // date|date-time|email

	re *regexp.Regexp
}

// AuthSpec: secrets may be literal, "env:VAR_NAME" or "sha256:<hex>".
type AuthSpec struct {
	Type       string            `json:"type"`             // none|bearer|api_key|basic
	Header     string            `json:"header,omitempty"` // api_key header, default X-API-Key
	Tokens     []string          `json:"tokens,omitempty"` // bearer / api_key
	Users      map[string]string `json:"users,omitempty"`  // basic: user -> password
	ReadPublic bool              `json:"read_public,omitempty"`

	secrets []secret
	users   map[string]secret
}

// APISpec is one JSON file = one API.
type APISpec struct {
	Name         string                `json:"name"`
	Version      string                `json:"version"`
	Description  string                `json:"description,omitempty"`
	Enabled      *bool                 `json:"enabled,omitempty"`
	IDField      string                `json:"id_field,omitempty"`
	Methods      []string              `json:"methods,omitempty"`
	Auth         AuthSpec              `json:"auth"`
	CIDRs        []string              `json:"cidrs,omitempty"`
	RateLimit    int                   `json:"rate_limit_per_min,omitempty"`
	CacheSeconds int                   `json:"cache_seconds,omitempty"`
	CORSOrigins  []string              `json:"cors_origins,omitempty"`
	StrictFields bool                  `json:"strict_fields,omitempty"`
	Fields       map[string]*FieldSpec `json:"fields,omitempty"`
	Data         []map[string]any      `json:"data"`

	file  string
	nets  []*net.IPNet
	index map[string]int
	etag  string
}

func (s *APISpec) key() string      { return s.Version + "/" + s.Name }
func (s *APISpec) enabled() bool    { return s.Enabled == nil || *s.Enabled }
func (s *APISpec) basePath() string { return "/" + s.Version + "/" + s.Name }

func (s *APISpec) allows(method string) bool {
	for _, m := range s.Methods {
		if m == method {
			return true
		}
	}
	return false
}

func (s *APISpec) normalize() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("name must match %s", nameRe)
	}
	if !versionRe.MatchString(s.Version) {
		return fmt.Errorf("version must look like v1, v2, v1-beta")
	}
	if s.IDField == "" {
		s.IDField = "id"
	}
	if len(s.Methods) == 0 {
		s.Methods = []string{"GET"}
	}
	for i, m := range s.Methods {
		m = strings.ToUpper(m)
		if !allMethods[m] {
			return fmt.Errorf("unsupported method %q", m)
		}
		s.Methods[i] = m
	}
	if s.RateLimit < 0 || s.CacheSeconds < 0 {
		return errors.New("rate_limit_per_min / cache_seconds must be >= 0")
	}
	if err := s.Auth.normalize(); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	s.nets = nil
	for _, c := range s.CIDRs {
		if !strings.Contains(c, "/") {
			if strings.Contains(c, ":") {
				c += "/128"
			} else {
				c += "/32"
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("bad cidr %q", c)
		}
		s.nets = append(s.nets, n)
	}
	for name, f := range s.Fields {
		if f == nil {
			return fmt.Errorf("field %q is null", name)
		}
		if f.Type == "" {
			f.Type = "any"
		}
		switch f.Type {
		case "string", "number", "integer", "boolean", "object", "array", "any":
		default:
			return fmt.Errorf("field %q: unknown type %q", name, f.Type)
		}
		if f.Pattern != "" {
			re, err := regexp.Compile(f.Pattern)
			if err != nil {
				return fmt.Errorf("field %q: bad pattern: %w", name, err)
			}
			f.re = re
		}
	}
	if s.Data == nil {
		s.Data = []map[string]any{}
	}
	for i, row := range s.Data {
		if err := s.validate(row, false); err != nil {
			return fmt.Errorf("data[%d]: %w", i, err)
		}
		if _, ok := row[s.IDField]; !ok {
			return fmt.Errorf("data[%d]: missing %q", i, s.IDField)
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

func (s *APISpec) allowsIP(ip net.IP) bool {
	if len(s.nets) == 0 {
		return true
	}
	for _, n := range s.nets {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
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
	for name, f := range s.Fields {
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
				return errors.New("must be RFC3339 date-time")
			}
		case "email":
			if at := strings.Index(*str, "@"); at < 1 || at == len(*str)-1 {
				return errors.New("must be an email")
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

func (a *AuthSpec) normalize() error {
	if a.Type == "" {
		a.Type = "none"
	}
	a.secrets, a.users = nil, nil
	switch a.Type {
	case "none":
	case "bearer", "api_key":
		if a.Type == "api_key" && a.Header == "" {
			a.Header = "X-API-Key"
		}
		if len(a.Tokens) == 0 {
			return errors.New("tokens required")
		}
		for _, t := range a.Tokens {
			sc, err := parseSecret(t)
			if err != nil {
				return err
			}
			a.secrets = append(a.secrets, sc)
		}
	case "basic":
		if len(a.Users) == 0 {
			return errors.New("users required")
		}
		a.users = map[string]secret{}
		for u, p := range a.Users {
			sc, err := parseSecret(p)
			if err != nil {
				return fmt.Errorf("user %s: %w", u, err)
			}
			a.users[u] = sc
		}
	default:
		return fmt.Errorf("unknown auth type %q", a.Type)
	}
	return nil
}

func (a *AuthSpec) check(r *http.Request) bool {
	anyMatch := func(got string) bool {
		ok := false
		for _, sc := range a.secrets {
			if got != "" && sc.match(got) {
				ok = true
			}
		}
		return ok
	}
	switch a.Type {
	case "none":
		return true
	case "bearer":
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			return false
		}
		return anyMatch(strings.TrimPrefix(h, "Bearer "))
	case "api_key":
		return anyMatch(r.Header.Get(a.Header))
	case "basic":
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Basic ") {
			return false
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
		if err != nil {
			return false
		}
		u, p, ok := strings.Cut(string(raw), ":")
		sc, found := a.users[u]
		return ok && found && sc.match(p)
	}
	return false
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
		return json.Number(fmt.Sprint(x)).String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}
