package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// API serves the generated REST APIs (the api listener).
type API struct {
	app     *App
	limiter *RateLimiter
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

// fail writes the public error shape: {"error": {"code", "message"}}.
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func failErr(w http.ResponseWriter, err error) {
	var he *httpErr
	switch {
	case errors.As(err, &he):
		fail(w, he.code, he.msg)
	case errors.Is(err, ErrInvalid):
		fail(w, 422, strings.TrimPrefix(err.Error(), "invalid: "))
	default:
		slog.Error("internal error", "kind", "system", "err", err)
		fail(w, 500, "internal error")
	}
}

// statusRec remembers the status and size of a response.
type statusRec struct {
	http.ResponseWriter
	code  int
	bytes int64
}

func (s *statusRec) WriteHeader(c int) {
	if s.code == 0 {
		s.code = c
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRec) Write(p []byte) (int, error) {
	if s.code == 0 {
		s.code = 200
	}
	n, err := s.ResponseWriter.Write(p)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRec) Unwrap() http.ResponseWriter { return s.ResponseWriter }

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-Id"); requestIDRe.MatchString(id) {
		return id
	}
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// reqInfo is filled while a request is handled and logged at the end.
type reqInfo struct {
	api, client, note string
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rid := requestID(r)
	w.Header().Set("X-Request-Id", rid)
	rec := &statusRec{ResponseWriter: w}
	info := &reqInfo{}
	a.route(rec, r, rid, info)
	if rec.code == 0 {
		rec.code = 200
	}
	if isHealthPath(r) && rec.code == 200 {
		return // probes would drown the logs
	}
	class := strconv.Itoa(rec.code/100) + "xx"
	a.app.metrics.Inc("fluid_api_requests_total", "api", info.api, "method", r.Method, "code", class)
	level := slog.LevelInfo
	if rec.code >= 500 {
		level = slog.LevelError
	} else if rec.code >= 400 {
		level = slog.LevelWarn
	}
	msg := r.Method + " " + r.URL.Path + " " + strconv.Itoa(rec.code)
	if info.note != "" {
		msg += ": " + info.note
	}
	slog.Log(r.Context(), level, msg, "kind", "api", "direction", "inbound", "api", info.api, "client", info.client,
		"method", r.Method, "url", r.URL.RequestURI(), "status", rec.code, "bytes_out", rec.bytes,
		"bytes_in", max(r.ContentLength, 0), "ms", time.Since(start).Milliseconds(),
		"source_ip", ClientIP(r, a.app.cfg.trusted).String(), "request_id", rid, "capture", captureFrom(r.Context()))
}

func (a *API) route(w http.ResponseWriter, r *http.Request, rid string, info *reqInfo) {
	app := a.app
	switch r.URL.Path {
	case "/livez":
		writeJSON(w, 200, map[string]any{"live": true})
		return
	case "/readyz":
		app.readyz(w, r)
		return
	case "/healthz":
		writeJSON(w, 200, map[string]any{"ok": true, "apis": len(app.store.All())})
		return
	case "/_catalog", "/openapi.json":
		info.api = strings.TrimPrefix(r.URL.Path, "/")
		if !a.catalogAllowed(w, r, info) {
			return
		}
		if r.URL.Path == "/_catalog" {
			writeJSON(w, 200, app.catalog())
		} else {
			writeJSON(w, 200, app.openAPI(r, app.cfg.APIBasePath))
		}
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 {
		fail(w, 404, "not found")
		return
	}
	spec := app.store.Get(parts[0] + "/" + parts[1])
	if spec == nil || !spec.enabled() {
		fail(w, 404, "api not found")
		return
	}
	info.api = spec.key()

	// CORS and preflight come before auth: browsers send preflights without credentials.
	if a.cors(w, r, spec) {
		return
	}
	ip := ClientIP(r, app.cfg.trusted)
	if _, ok := spec.allowsIP(ip); !ok {
		app.metrics.Inc("fluid_access_denied_total", "listener", "api")
		info.note = "source IP not allowed"
		if spec.restrictedDefault() {
			info.note += " (no allowed_ips: only loopback and private networks may call it)"
		}
		fail(w, 403, "forbidden: your address is not allowed")
		return
	}
	if ok, wait := a.limiter.Allow(spec.key()+"|"+ip.String(), spec.limit); !ok {
		app.metrics.Inc("fluid_rate_limited_total", "api", spec.key())
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		info.note = "rate limited"
		fail(w, 429, "rate limit exceeded")
		return
	}
	if !spec.allows(r.Method) {
		w.Header().Set("Allow", strings.Join(spec.Methods, ", "))
		fail(w, 405, "method not allowed")
		return
	}
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	who := actor{via: "api", ip: ip.String(), requestID: rid}
	if read && spec.Auth.ReadPublic {
		who.name = "anonymous"
	} else {
		name, ok := spec.Auth.check(r)
		if !ok {
			switch spec.Auth.Type {
			case "bearer":
				w.Header().Set("WWW-Authenticate", `Bearer realm="fluid-config"`)
			case "basic":
				w.Header().Set("WWW-Authenticate", `Basic realm="fluid-config"`)
			}
			info.note = "bad or missing credentials"
			fail(w, 401, "unauthorized")
			return
		}
		who.name = name
	}
	info.client = who.name
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, app.cfg.MaxBodyBytes)
	}

	if read && a.serveCached(w, r, spec) {
		return
	}
	if len(parts) == 2 {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			a.cached(w, r, spec, func(w http.ResponseWriter) { a.list(w, r, spec) })
		case http.MethodPost:
			a.create(w, r, spec, who)
		default:
			fail(w, 405, "use "+spec.basePath()+"/{id} for "+r.Method)
		}
		return
	}
	id := parts[2]
	if id == "_schema" && read {
		a.cached(w, r, spec, func(w http.ResponseWriter) { writeJSON(w, 200, schemaView(spec)) })
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		a.cached(w, r, spec, func(w http.ResponseWriter) { a.get(w, r, spec, id) })
	case http.MethodPut:
		a.replace(w, r, spec, id, who)
	case http.MethodPatch:
		a.patch(w, r, spec, id, who)
	case http.MethodDelete:
		if err := app.deleteRecord(r.Context(), spec.key(), id, r.Header.Get("If-Match"), who); err != nil {
			failErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, 405, "method not allowed")
	}
}

// ---------- in-memory cache ----------

func cacheKey(r *http.Request, spec *APISpec) string {
	return spec.etag + "|" + r.URL.Path + "?" + r.URL.Query().Encode()
}

// serveCached answers from the cache when the API caches and the entry is fresh.
func (a *API) serveCached(w http.ResponseWriter, r *http.Request, spec *APISpec) bool {
	if !spec.cacheOn() || r.Header.Get("Cache-Control") == "no-cache" {
		return false
	}
	e := a.app.cache.Get(spec.key(), cacheKey(r, spec))
	if e == nil {
		return false
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == e.header.Get("ETag") {
		copyHeaders(w.Header(), e.header)
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	copyHeaders(w.Header(), e.header)
	w.Header().Set("X-Cache", "HIT")
	w.WriteHeader(e.status)
	w.Write(e.body)
	return true
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
}

// cached renders a response and, for APIs that cache, keeps a 200 for next time.
func (a *API) cached(w http.ResponseWriter, r *http.Request, spec *APISpec, render func(http.ResponseWriter)) {
	if !spec.cacheOn() {
		render(w)
		return
	}
	w.Header().Set("X-Cache", "MISS")
	bw := &bufferWriter{ResponseWriter: w}
	render(bw)
	if bw.status == 200 && r.Method == http.MethodGet {
		h := w.Header().Clone()
		h.Del("X-Request-Id")
		h.Del("X-Cache")
		a.app.cache.Put(spec.key(), spec.Cache, &cachedResponse{key: cacheKey(r, spec), status: 200, header: h, body: bw.body})
	}
}

// ---------- read ----------

func (a *API) cacheHeaders(w http.ResponseWriter, r *http.Request, spec *APISpec, etag string) bool {
	w.Header().Set("ETag", etag)
	if spec.CacheSeconds > 0 {
		scope := "public"
		if spec.Auth.Type != "none" && !spec.Auth.ReadPublic {
			scope = "private"
		}
		w.Header().Set("Cache-Control", fmt.Sprintf("%s, max-age=%d", scope, spec.CacheSeconds))
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func (a *API) list(w http.ResponseWriter, r *http.Request, spec *APISpec) {
	rq, err := parsePublicQuery(r.URL.Query(), a.app.cfg.MaxPageSize)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if a.cacheHeaders(w, r, spec, spec.etag) {
		return
	}
	all, page := rq.run(spec.Data, spec.IDField)
	total := len(all)
	writeJSON(w, 200, map[string]any{
		"data": page,
		"meta": map[string]any{
			"api": spec.Name, "version": spec.Version,
			"total": total, "page": rq.offset/rq.limit + 1, "limit": rq.limit,
			"pages": (total + rq.limit - 1) / rq.limit,
		},
	})
}

func (a *API) get(w http.ResponseWriter, r *http.Request, spec *APISpec, id string) {
	row, ok := spec.record(id)
	if !ok {
		fail(w, 404, "record not found")
		return
	}
	if a.cacheHeaders(w, r, spec, recordETag(row)) {
		return
	}
	var fields []string
	if f := r.URL.Query().Get("fields"); f != "" {
		fields = strings.Split(f, ",")
	}
	writeJSON(w, 200, map[string]any{"data": project(row, fields, spec.IDField)})
}

// ---------- write ----------

func decodeObject(r *http.Request) (map[string]any, error) {
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &httpErr{413, "body too large"}
		}
		return nil, &httpErr{400, "body must be a JSON object"}
	}
	if dec.More() {
		return nil, &httpErr{400, "body must be one JSON object"}
	}
	return m, nil
}

func (a *API) create(w http.ResponseWriter, r *http.Request, spec *APISpec, who actor) {
	body, err := decodeObject(r)
	if err != nil {
		failErr(w, err)
		return
	}
	row, n, err := a.app.createRecord(r.Context(), spec.key(), body, who)
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("Location", a.app.cfg.APIBasePath+n.basePath()+"/"+toString(row[n.IDField]))
	w.Header().Set("ETag", recordETag(row))
	writeJSON(w, 201, map[string]any{"data": row})
}

func (a *API) replace(w http.ResponseWriter, r *http.Request, spec *APISpec, id string, who actor) {
	body, err := decodeObject(r)
	if err != nil {
		failErr(w, err)
		return
	}
	row, created, _, err := a.app.replaceRecord(r.Context(), spec.key(), id, body, r.Header.Get("If-Match"), who)
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("ETag", recordETag(row))
	code := 200
	if created {
		code = 201
	}
	writeJSON(w, code, map[string]any{"data": row})
}

func (a *API) patch(w http.ResponseWriter, r *http.Request, spec *APISpec, id string, who actor) {
	body, err := decodeObject(r)
	if err != nil {
		failErr(w, err)
		return
	}
	row, _, err := a.app.patchRecord(r.Context(), spec.key(), id, body, r.Header.Get("If-Match"), who)
	if err != nil {
		failErr(w, err)
		return
	}
	w.Header().Set("ETag", recordETag(row))
	writeJSON(w, 200, map[string]any{"data": row})
}

// ---------- cross-cutting ----------

// cors sets headers; returns true if it fully handled a preflight.
func (a *API) cors(w http.ResponseWriter, r *http.Request, spec *APISpec) bool {
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
	w.Header().Set("Access-Control-Expose-Headers", "ETag, Location, X-Request-Id, X-Cache")
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		w.Header().Set("Access-Control-Allow-Methods", strings.Join(spec.Methods, ", "))
		hdrs := "Authorization, Content-Type, If-None-Match, If-Match, X-Request-Id"
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

// catalogAllowed guards /_catalog and /openapi.json: the access.json catalog
// allowlist, then any admin token unless public_catalog is on.
func (a *API) catalogAllowed(w http.ResponseWriter, r *http.Request, info *reqInfo) bool {
	ip := ClientIP(r, a.app.cfg.trusted)
	if _, ok := a.app.store.Access().Catalog.list.Match(ip); !ok {
		info.note = "source IP not in the catalog allowlist"
		fail(w, 403, "forbidden: your address is not allowed")
		return false
	}
	if a.app.cfg.PublicCatalog {
		return true
	}
	p, ok := a.app.admin.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="fluid-config"`)
		info.note = "the catalog needs an admin token (viewer or editor)"
		fail(w, 401, "unauthorized: use a viewer or editor token from admin.json")
		return false
	}
	info.client = "admin:" + p.Name
	return true
}

func (app *App) catalog() map[string]any {
	list := []map[string]any{}
	for _, a := range app.store.All() {
		list = append(list, map[string]any{
			"name": a.Name, "version": a.Version, "description": a.Description,
			"enabled": a.enabled(), "path": app.cfg.APIBasePath + a.basePath(),
			"methods": a.Methods, "auth": a.Auth.Type, "read_public": a.Auth.ReadPublic,
			"records": len(a.Data), "public": a.public(), "cache": a.cacheOn(),
		})
	}
	return map[string]any{"apis": list}
}

// schemaView exposes the contract without data or secrets.
func schemaView(a *APISpec) map[string]any {
	return map[string]any{
		"name": a.Name, "version": a.Version, "description": a.Description,
		"id_field": a.IDField, "methods": a.Methods, "auth": a.Auth.Type, "read_public": a.Auth.ReadPublic,
		"strict_fields": a.StrictFields, "fields": orEmptyMap(a.Fields), "records": len(a.Data),
		"max_records": a.MaxRecords,
	}
}

func orEmptyMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return map[K]V{}
	}
	return m
}
