package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

type Server struct {
	store   *Store
	cfg     *StaticConfig
	limiter *limiter
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func failErr(w http.ResponseWriter, err error) {
	var he *httpErr
	if errors.As(err, &he) {
		fail(w, he.code, he.msg)
		return
	}
	slog.Error("internal error", "err", err)
	fail(w, 500, "internal error")
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRec{ResponseWriter: w, code: 200}
	s.route(rec, r)
	slog.Info("req", "method", r.Method, "path", r.URL.Path, "status", rec.code,
		"ip", s.clientIP(r).String(), "ms", time.Since(start).Milliseconds())
}

type statusRec struct {
	http.ResponseWriter
	code int
}

func (s *statusRec) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if s.cfg.BasePath != "" {
		if !strings.HasPrefix(path, s.cfg.BasePath+"/") {
			fail(w, 404, "not found")
			return
		}
		path = strings.TrimPrefix(path, s.cfg.BasePath)
	}
	switch path {
	case "/healthz":
		writeJSON(w, 200, map[string]any{"ok": true, "apis": len(s.store.All())})
		return
	case "/_catalog":
		if s.catalogAuth(w, r) {
			writeJSON(w, 200, s.catalog())
		}
		return
	case "/openapi.json":
		if s.catalogAuth(w, r) {
			writeJSON(w, 200, s.openAPI(r))
		}
		return
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 {
		fail(w, 404, "not found")
		return
	}
	spec := s.store.Get(parts[0] + "/" + parts[1])
	if spec == nil || !spec.enabled() {
		fail(w, 404, "api not found")
		return
	}

	// CORS + preflight before auth (browsers send preflight without credentials)
	if s.cors(w, r, spec) {
		return
	}
	ip := s.clientIP(r)
	if !spec.allowsIP(ip) {
		fail(w, 403, "ip not allowed")
		return
	}
	if spec.RateLimit > 0 && !s.limiter.allow(spec.key()+"|"+ip.String(), spec.RateLimit) {
		w.Header().Set("Retry-After", strconv.Itoa(60-time.Now().Second()))
		fail(w, 429, "rate limit exceeded")
		return
	}
	if !spec.allows(r.Method) {
		w.Header().Set("Allow", strings.Join(spec.Methods, ", "))
		fail(w, 405, "method not allowed")
		return
	}
	if !(spec.Auth.ReadPublic && r.Method == http.MethodGet) && !spec.Auth.check(r) {
		switch spec.Auth.Type {
		case "bearer":
			w.Header().Set("WWW-Authenticate", `Bearer realm="fluid-config"`)
		case "basic":
			w.Header().Set("WWW-Authenticate", `Basic realm="fluid-config"`)
		}
		fail(w, 401, "unauthorized")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	}

	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet:
			s.list(w, r, spec)
		case http.MethodPost:
			s.create(w, r, spec)
		default:
			fail(w, 405, "use /"+parts[0]+"/"+parts[1]+"/{id} for "+r.Method)
		}
		return
	}
	id := parts[2]
	if id == "_schema" && r.Method == http.MethodGet {
		writeJSON(w, 200, schemaView(spec))
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.get(w, r, spec, id)
	case http.MethodPut:
		s.replace(w, r, spec, id)
	case http.MethodPatch:
		s.patch(w, r, spec, id)
	case http.MethodDelete:
		s.del(w, spec, id)
	default:
		fail(w, 405, "method not allowed")
	}
}

// ---------- read ----------

func (s *Server) cacheHeaders(w http.ResponseWriter, r *http.Request, spec *APISpec) bool {
	w.Header().Set("ETag", spec.etag)
	if spec.CacheSeconds > 0 {
		scope := "public"
		if spec.Auth.Type != "none" && !spec.Auth.ReadPublic {
			scope = "private"
		}
		w.Header().Set("Cache-Control", fmt.Sprintf("%s, max-age=%d", scope, spec.CacheSeconds))
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == spec.etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

var reserved = map[string]bool{"q": true, "sort": true, "page": true, "limit": true, "fields": true}

type filter struct {
	field, op string
	vals      []string
}

func parseFilters(r *http.Request) ([]filter, error) {
	var fs []filter
	for k, vals := range r.URL.Query() {
		if reserved[k] {
			continue
		}
		field, op := k, "eq"
		if i := strings.IndexByte(k, '['); i > 0 && strings.HasSuffix(k, "]") {
			field, op = k[:i], k[i+1:len(k)-1]
		}
		switch op {
		case "eq", "ne", "gt", "gte", "lt", "lte", "like", "in", "exists":
		default:
			return nil, fmt.Errorf("unknown operator %q (eq ne gt gte lt lte like in exists)", op)
		}
		for _, v := range vals {
			f := filter{field: field, op: op, vals: []string{v}}
			if op == "in" {
				f.vals = strings.Split(v, ",")
			}
			fs = append(fs, f)
		}
	}
	return fs, nil
}

func getPath(row map[string]any, path string) (any, bool) {
	var cur any = row
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// compare: numeric when both sides parse as numbers, else string.
func compare(a, b string) int {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea == nil && eb == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

func (f filter) match(row map[string]any) bool {
	v, ok := getPath(row, f.field)
	if f.op == "exists" {
		want := f.vals[0] != "false"
		return ok == want
	}
	if !ok {
		return f.op == "ne"
	}
	sv := toString(v)
	switch f.op {
	case "eq":
		return compare(sv, f.vals[0]) == 0
	case "ne":
		return compare(sv, f.vals[0]) != 0
	case "gt":
		return compare(sv, f.vals[0]) > 0
	case "gte":
		return compare(sv, f.vals[0]) >= 0
	case "lt":
		return compare(sv, f.vals[0]) < 0
	case "lte":
		return compare(sv, f.vals[0]) <= 0
	case "like":
		return strings.Contains(strings.ToLower(sv), strings.ToLower(f.vals[0]))
	case "in":
		for _, x := range f.vals {
			if compare(sv, strings.TrimSpace(x)) == 0 {
				return true
			}
		}
	}
	return false
}

func matchQ(row map[string]any, q string) bool {
	for _, v := range row {
		if strings.Contains(strings.ToLower(toString(v)), q) {
			return true
		}
	}
	return false
}

func project(row map[string]any, fields []string, idField string) map[string]any {
	if len(fields) == 0 {
		return row
	}
	out := map[string]any{idField: row[idField]}
	for _, f := range fields {
		if v, ok := getPath(row, f); ok {
			out[f] = v
		}
	}
	return out
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, spec *APISpec) {
	q := r.URL.Query()
	filters, err := parseFilters(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	limit, page := 50, 1
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			fail(w, 400, "limit must be a positive integer")
			return
		}
	}
	if limit > s.cfg.MaxPageSize {
		limit = s.cfg.MaxPageSize
	}
	if v := q.Get("page"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 1 {
			fail(w, 400, "page must be a positive integer")
			return
		}
	}
	if s.cacheHeaders(w, r, spec) {
		return
	}

	search := strings.ToLower(q.Get("q"))
	out := make([]map[string]any, 0)
rows:
	for _, row := range spec.Data {
		for _, f := range filters {
			if !f.match(row) {
				continue rows
			}
		}
		if search != "" && !matchQ(row, search) {
			continue
		}
		out = append(out, row)
	}

	if sortSpec := q.Get("sort"); sortSpec != "" {
		keys := strings.Split(sortSpec, ",")
		sort.SliceStable(out, func(i, j int) bool {
			for _, k := range keys {
				desc := strings.HasPrefix(k, "-")
				k = strings.TrimPrefix(k, "-")
				a, aok := getPath(out[i], k)
				b, bok := getPath(out[j], k)
				if aok != bok {
					return aok // missing values last
				}
				c := compare(toString(a), toString(b))
				if c == 0 {
					continue
				}
				if desc {
					return c > 0
				}
				return c < 0
			}
			return false
		})
	}

	total := len(out)
	from := (page - 1) * limit
	if from > total {
		from = total
	}
	to := from + limit
	if to > total {
		to = total
	}
	var fields []string
	if f := q.Get("fields"); f != "" {
		fields = strings.Split(f, ",")
	}
	pageRows := make([]map[string]any, 0, to-from)
	for _, row := range out[from:to] {
		pageRows = append(pageRows, project(row, fields, spec.IDField))
	}
	writeJSON(w, 200, map[string]any{
		"data": pageRows,
		"meta": map[string]any{
			"api": spec.Name, "version": spec.Version,
			"total": total, "page": page, "limit": limit,
			"pages": (total + limit - 1) / limit,
		},
	})
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, spec *APISpec, id string) {
	i, ok := spec.index[id]
	if !ok {
		fail(w, 404, "record not found")
		return
	}
	if s.cacheHeaders(w, r, spec) {
		return
	}
	var fields []string
	if f := r.URL.Query().Get("fields"); f != "" {
		fields = strings.Split(f, ",")
	}
	writeJSON(w, 200, map[string]any{"data": project(spec.Data[i], fields, spec.IDField)})
}

// ---------- write ----------

func decodeObject(r *http.Request) (map[string]any, error) {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, &httpErr{400, "body must be a JSON object"}
	}
	return m, nil
}

func nextID(spec *APISpec) any {
	max := new(big.Int)
	numeric := len(spec.Data) > 0
	for _, row := range spec.Data {
		n, ok := new(big.Int).SetString(toString(row[spec.IDField]), 10)
		if !ok {
			numeric = false
			break
		}
		if n.Cmp(max) > 0 {
			max = n
		}
	}
	if numeric {
		return json.Number(max.Add(max, big.NewInt(1)).String())
	}
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, spec *APISpec) {
	body, err := decodeObject(r)
	if err != nil {
		failErr(w, err)
		return
	}
	var created map[string]any
	n, err := s.store.Mutate(spec.key(), func(cur *APISpec) ([]map[string]any, error) {
		if _, has := body[cur.IDField]; !has || body[cur.IDField] == nil {
			body[cur.IDField] = nextID(cur)
		}
		id := toString(body[cur.IDField])
		if strings.ContainsAny(id, "/?#") || id == "_schema" {
			return nil, &httpErr{400, "invalid id"}
		}
		if _, dup := cur.index[id]; dup {
			return nil, &httpErr{409, "record already exists"}
		}
		if err := cur.validate(body, false); err != nil {
			return nil, &httpErr{422, err.Error()}
		}
		created = body
		return append(append(make([]map[string]any, 0, len(cur.Data)+1), cur.Data...), body), nil
	})
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("Location", s.cfg.BasePath+n.basePath()+"/"+toString(created[n.IDField]))
	w.Header().Set("ETag", n.etag)
	writeJSON(w, 201, map[string]any{"data": created})
}

func (s *Server) replace(w http.ResponseWriter, r *http.Request, spec *APISpec, id string) {
	body, err := decodeObject(r)
	if err != nil {
		failErr(w, err)
		return
	}
	code := 200
	n, err := s.store.Mutate(spec.key(), func(cur *APISpec) ([]map[string]any, error) {
		if v, has := body[cur.IDField]; has && toString(v) != id {
			return nil, &httpErr{400, "id in body does not match URL"}
		}
		if _, has := body[cur.IDField]; !has {
			if i, ok := cur.index[id]; ok {
				body[cur.IDField] = cur.Data[i][cur.IDField] // keep original type
			} else {
				body[cur.IDField] = id
			}
		}
		if err := cur.validate(body, false); err != nil {
			return nil, &httpErr{422, err.Error()}
		}
		data := append(make([]map[string]any, 0, len(cur.Data)+1), cur.Data...)
		if i, ok := cur.index[id]; ok {
			data[i] = body
		} else {
			data = append(data, body) // upsert
			code = 201
		}
		return data, nil
	})
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("ETag", n.etag)
	writeJSON(w, code, map[string]any{"data": body})
}

// PATCH uses JSON Merge Patch semantics: send null to remove a field.
func (s *Server) patch(w http.ResponseWriter, r *http.Request, spec *APISpec, id string) {
	body, err := decodeObject(r)
	if err != nil {
		failErr(w, err)
		return
	}
	var merged map[string]any
	n, err := s.store.Mutate(spec.key(), func(cur *APISpec) ([]map[string]any, error) {
		i, ok := cur.index[id]
		if !ok {
			return nil, &httpErr{404, "record not found"}
		}
		if v, has := body[cur.IDField]; has && toString(v) != id {
			return nil, &httpErr{400, "id cannot be changed"}
		}
		merged = make(map[string]any, len(cur.Data[i])+len(body))
		for k, v := range cur.Data[i] {
			merged[k] = v
		}
		for k, v := range body {
			if v == nil {
				delete(merged, k)
			} else {
				merged[k] = v
			}
		}
		if err := cur.validate(merged, false); err != nil {
			return nil, &httpErr{422, err.Error()}
		}
		data := append(make([]map[string]any, 0, len(cur.Data)), cur.Data...)
		data[i] = merged
		return data, nil
	})
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("ETag", n.etag)
	writeJSON(w, 200, map[string]any{"data": merged})
}

func (s *Server) del(w http.ResponseWriter, spec *APISpec, id string) {
	_, err := s.store.Mutate(spec.key(), func(cur *APISpec) ([]map[string]any, error) {
		i, ok := cur.index[id]
		if !ok {
			return nil, &httpErr{404, "record not found"}
		}
		data := make([]map[string]any, 0, len(cur.Data)-1)
		data = append(data, cur.Data[:i]...)
		return append(data, cur.Data[i+1:]...), nil
	})
	if err != nil {
		failErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- cross-cutting ----------

func (s *Server) clientIP(r *http.Request) net.IP {
	if s.cfg.TrustForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip
			}
		}
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return net.ParseIP(h)
}

// cors sets headers; returns true if it fully handled a preflight.
func (s *Server) cors(w http.ResponseWriter, r *http.Request, spec *APISpec) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || len(spec.CORSOrigins) == 0 {
		return false
	}
	allowed := false
	for _, o := range spec.CORSOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			allowed = true
		}
	}
	if !allowed {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Add("Vary", "Origin")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		w.Header().Set("Access-Control-Allow-Methods", strings.Join(spec.Methods, ", "))
		hdrs := "Authorization, Content-Type, If-None-Match"
		if spec.Auth.Header != "" {
			hdrs += ", " + spec.Auth.Header
		}
		w.Header().Set("Access-Control-Allow-Headers", hdrs)
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func (s *Server) catalogAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.CatalogToken == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.CatalogToken)) != 1 {
		fail(w, 401, "unauthorized")
		return false
	}
	return true
}

func (s *Server) catalog() map[string]any {
	var list []map[string]any
	for _, a := range s.store.All() {
		list = append(list, map[string]any{
			"name": a.Name, "version": a.Version, "description": a.Description,
			"enabled": a.enabled(), "path": s.cfg.BasePath + a.basePath(),
			"methods": a.Methods, "auth": a.Auth.Type, "read_public": a.Auth.ReadPublic,
			"records": len(a.Data), "ip_restricted": len(a.CIDRs) > 0,
			"rate_limit_per_min": a.RateLimit,
		})
	}
	return map[string]any{"apis": list}
}

// schemaView exposes the contract without data or secrets.
func schemaView(a *APISpec) map[string]any {
	return map[string]any{
		"name": a.Name, "version": a.Version, "description": a.Description,
		"id_field": a.IDField, "methods": a.Methods, "auth": a.Auth.Type,
		"strict_fields": a.StrictFields, "fields": a.Fields, "records": len(a.Data),
	}
}

// ---------- rate limiter: fixed 1-minute window per api+ip ----------

type limiter struct {
	mu     sync.Mutex
	window int64
	counts map[string]int
}

func newLimiter() *limiter { return &limiter{counts: map[string]int{}} }

func (l *limiter) allow(key string, perMin int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w := time.Now().Unix() / 60; w != l.window {
		l.window, l.counts = w, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= perMin
}
