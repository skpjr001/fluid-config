package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Mask replaces secrets in API responses. Sending it back unchanged keeps
// the stored value, so the UI can round-trip objects without seeing secrets.
const Mask = "********"

type ctxKey int

const principalKey ctxKey = 0

type Principal struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
}

func principalOf(r *http.Request) Principal {
	p, _ := r.Context().Value(principalKey).(Principal)
	return p
}

// Admin serves the console and its API (the admin listener).
type Admin struct {
	app *App
}

func (a *Admin) tokens() []AdminToken {
	ts := slices.Clone(a.app.store.Admin().Tokens)
	if env := envAdminToken(); env != "" {
		ts = append(ts, AdminToken{Name: "env", Token: env, Role: RoleEditor})
	}
	return ts
}

func (a *Admin) authenticate(r *http.Request) (Principal, bool) {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		return Principal{}, false
	}
	var p Principal
	ok := false
	for _, t := range a.tokens() { // compare all, constant time each
		if ctEqual(got, t.Token) {
			p, ok = Principal{Name: t.Name, Role: t.Role}, true
		}
	}
	return p, ok
}

func (a *Admin) who(r *http.Request) actor {
	return actor{name: principalOf(r).Name, via: "console", ip: ClientIP(r, a.app.cfg.trusted).String(), requestID: requestID(r)}
}

func (a *Admin) Handler() http.Handler {
	api := http.NewServeMux()
	route := func(pattern string, editor bool, h http.HandlerFunc) { api.Handle(pattern, a.guard(editor, h)) }
	route("GET /api/whoami", false, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, principalOf(r)) })
	route("GET /api/status", false, a.status)
	route("POST /api/reload", true, a.reload)
	route("GET /metrics", false, a.metrics)
	api.HandleFunc("GET /api/schema/{name}", a.schema) // public: the schemas are published anyway
	route("GET /api/openapi.json", false, func(w http.ResponseWriter, r *http.Request) {
		doc := a.app.openAPI(r, a.app.cfg.APIBasePath)
		doc["servers"] = []any{map[string]any{"url": a.apiURL(r)}}
		writeJSON(w, 200, doc)
	})

	route("GET /api/apis", false, a.listAPIs)
	route("POST /api/apis", true, a.createAPI)
	route("POST /api/validate", false, a.validateAPI)
	route("POST /api/tokens/generate", true, a.generateToken)
	route("GET /api/apis/{v}/{n}", false, a.getAPI)
	route("PUT /api/apis/{v}/{n}", true, a.putAPI)
	route("DELETE /api/apis/{v}/{n}", true, a.deleteAPI)
	route("POST /api/apis/{v}/{n}/cache/purge", true, a.purgeCache)
	route("GET /api/apis/{v}/{n}/records", false, a.listRecords)
	route("POST /api/apis/{v}/{n}/records", true, a.createRecord)
	route("GET /api/apis/{v}/{n}/records/{id}", false, a.getRecord)
	route("PUT /api/apis/{v}/{n}/records/{id}", true, a.putRecord)
	route("PATCH /api/apis/{v}/{n}/records/{id}", true, a.patchRecord)
	route("DELETE /api/apis/{v}/{n}/records/{id}", true, a.deleteRecord)
	route("POST /api/apis/{v}/{n}/import", true, a.importRecords)
	route("GET /api/apis/{v}/{n}/export", false, a.exportRecords)

	route("GET /api/audit", false, a.listAudit)
	route("GET /api/audit/{id}", false, a.getAudit)
	route("POST /api/audit/{id}/revert", true, a.revertAudit)

	route("GET /api/access", false, a.getAccess)
	route("PUT /api/access", true, a.putAccess)
	route("POST /api/access/test", false, a.testAccess)

	route("GET /api/logs", false, a.listLogs)
	route("GET /api/logs/summary", false, a.summaryLogs)
	route("GET /api/logs/{id}", false, a.getLog)
	route("GET /api/stream", false, a.stream)

	root := http.NewServeMux()
	root.Handle("/api/", api)
	root.Handle("/metrics", api)
	root.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]any{"live": true}) })
	root.HandleFunc("GET /readyz", a.app.readyz)
	ui, _ := fs.Sub(uiFS, "ui")
	root.Handle("/", a.uiHandler(ui))
	return root
}

// maxAdminBody allows imports of large datasets through the console.
const maxAdminBody = 32 << 20

func (a *Admin) guard(editor bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.authenticate(r)
		if !ok {
			a.app.metrics.Inc("fluid_admin_requests_total", "role", "", "result", "unauthorized")
			slog.Warn("admin auth failed", "kind", "admin", "source_ip", ClientIP(r, a.app.cfg.trusted).String(),
				"method", r.Method, "url", r.URL.Path, "status", 401)
			errJSON(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if editor && p.Role != RoleEditor {
			a.app.metrics.Inc("fluid_admin_requests_total", "role", string(p.Role), "result", "forbidden")
			errJSON(w, http.StatusForbidden, "this action needs an editor token")
			return
		}
		a.app.metrics.Inc("fluid_admin_requests_total", "role", string(p.Role), "result", "ok")
		if r.Method != http.MethodGet {
			slog.Info("admin change", "kind", "admin", "client", p.Name, "method", r.Method, "url", r.URL.Path,
				"source_ip", ClientIP(r, a.app.cfg.trusted).String())
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func errJSON(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// adminErr maps store and record errors to console answers.
func adminErr(w http.ResponseWriter, err error) {
	var he *httpErr
	switch {
	case errors.As(err, &he):
		errJSON(w, he.code, he.msg)
	case errors.Is(err, ErrInvalid):
		errJSON(w, 422, strings.TrimPrefix(err.Error(), "invalid: "))
	default:
		slog.Error("admin request failed", "kind", "admin", "err", err)
		errJSON(w, 500, err.Error())
	}
}

// apiURL guesses the public base URL of the API listener for examples.
func (a *Admin) apiURL(r *http.Request) string {
	scheme := "http"
	if a.app.cfg.APITLS != nil {
		scheme = "https"
	}
	host, _, _ := strings.Cut(r.Host, ":")
	_, port, _ := strings.Cut(a.app.cfg.APIAddr, ":")
	if port == "" || (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return scheme + "://" + host + a.app.cfg.APIBasePath
	}
	return scheme + "://" + host + ":" + port + a.app.cfg.APIBasePath
}

// ---------- status / metrics / schema ----------

func (a *Admin) status(w http.ResponseWriter, r *http.Request) {
	st := a.app.store
	var dbSize int64
	if fi, err := os.Stat(a.app.db.path); err == nil {
		dbSize = fi.Size()
	}
	records, cached := 0, 0
	for _, s := range st.All() {
		records += len(s.Data)
		if s.cacheOn() {
			cached++
		}
	}
	writeJSON(w, 200, map[string]any{
		"version":    version,
		"started_at": a.app.started.UTC(),
		"uptime_sec": int64(time.Since(a.app.started).Seconds()),
		"listeners":  map[string]string{"api": a.app.cfg.APIAddr, "admin": a.app.cfg.AdminAddr},
		"api_url":    a.apiURL(r),
		"config": map[string]any{
			"dir": a.app.cfg.DynamicDir, "loaded_at": st.LoadedAt().UTC(), "errors": st.Errors(),
			"apis": len(st.All()), "records": records, "cached_apis": cached,
		},
		"cache":        map[string]any{"bytes": a.app.cache.TotalBytes()},
		"database":     map[string]any{"path": a.app.db.path, "size_bytes": dbSize, "retention_days": a.app.cfg.Database.RetentionDays, "audit_retention_days": a.app.cfg.Database.AuditRetentionDays},
		"logs_dropped": a.app.sink.Dropped(),
		"environment":  map[string]any{"container": a.app.env.Container, "systemd": a.app.env.Systemd},
		"deployment":   a.currentNotes(),
	})
}

// currentNotes is the startup notes with the access section recomputed, so
// the Status page reflects changes made since startup.
func (a *Admin) currentNotes() []DeployNote {
	out := []DeployNote{}
	for _, n := range a.app.notes {
		if n.Topic != "access" {
			out = append(out, n)
		}
	}
	return append(out, accessNotes(a.app.cfg, a.app.store.snap.Load(), a.app.env)...)
}

func (a *Admin) reload(w http.ResponseWriter, r *http.Request) {
	if err := a.app.store.Reload(true); err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"reloaded": true, "errors": a.app.store.Errors()})
}

func (a *Admin) metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	a.app.metrics.WritePrometheus(w)
}

func (a *Admin) schema(w http.ResponseWriter, r *http.Request) {
	b, err := schemaFS.ReadFile("schema/" + r.PathValue("name"))
	if err != nil {
		errJSON(w, 404, "no such schema")
		return
	}
	w.Header().Set("Content-Type", "application/schema+json")
	w.Write(b)
}

// ---------- APIs (contracts) ----------

type apiSummary struct {
	Key          string     `json:"key"`
	Name         string     `json:"name"`
	Version      string     `json:"version"`
	Description  string     `json:"description"`
	Enabled      bool       `json:"enabled"`
	Path         string     `json:"path"`
	File         string     `json:"file"`
	ModTime      time.Time  `json:"mod_time,omitzero"`
	Methods      []string   `json:"methods"`
	Auth         string     `json:"auth"`
	ReadPublic   bool       `json:"read_public"`
	AllowedIPs   []string   `json:"allowed_ips"` // allowed_ips + cidrs, or the restricted default
	Default      bool       `json:"default"`     // no allowed_ips/cidrs: restricted default
	Public       bool       `json:"public"`      // reachable from the internet
	Records      int        `json:"records"`
	Fields       int        `json:"fields"`
	IDField      string     `json:"id_field"`
	MaxRecords   int        `json:"max_records,omitempty"`
	CacheSeconds int        `json:"cache_seconds"`
	Cache        CacheStats `json:"cache"`
	Error        string     `json:"error,omitempty"` // file currently invalid: the previous version is served
	Changes24h   int64      `json:"changes_24h"`
}

func (a *Admin) summary(s *APISpec, changes map[string]int64) apiSummary {
	ips := s.allowedIPs()
	if s.restrictedDefault() {
		ips = defaultAllow
	}
	return apiSummary{Key: s.key(), Name: s.Name, Version: s.Version, Description: s.Description, Enabled: s.enabled(),
		Path: a.app.cfg.APIBasePath + s.basePath(), File: apisSubdir + "/" + s.rel, ModTime: s.modTime, Methods: s.Methods,
		Auth: s.Auth.Type, ReadPublic: s.Auth.ReadPublic, AllowedIPs: ips, Default: s.restrictedDefault(), Public: s.public(),
		Records: len(s.Data), Fields: len(s.Fields), IDField: s.IDField, MaxRecords: s.MaxRecords, CacheSeconds: s.CacheSeconds,
		Cache: a.app.cache.Stats(s), Error: a.app.store.Errors()[apisSubdir+"/"+s.rel], Changes24h: changes[s.key()]}
}

func (a *Admin) listAPIs(w http.ResponseWriter, r *http.Request) {
	changes, _ := a.app.audit.CountsSince(r.Context(), time.Now().Add(-24*time.Hour))
	out := []apiSummary{}
	for _, s := range a.app.store.All() {
		out = append(out, a.summary(s, changes))
	}
	writeJSON(w, 200, out)
}

func (a *Admin) spec(w http.ResponseWriter, r *http.Request) *APISpec {
	s := a.app.store.Get(r.PathValue("v") + "/" + r.PathValue("n"))
	if s == nil {
		errJSON(w, 404, "api not found")
	}
	return s
}

func (a *Admin) getAPI(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	changes, _ := a.app.audit.CountsSince(r.Context(), time.Now().Add(-24*time.Hour))
	writeJSON(w, 200, map[string]any{"summary": a.summary(s, changes), "contract": maskedContract(s),
		"columns": columns(s), "etag": s.etag, "api_url": a.apiURL(r)})
}

// columns lists the record fields: declared ones first, then any others seen.
func columns(s *APISpec) []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	add := func(name string, f *FieldSpec) {
		if seen[name] {
			return
		}
		seen[name] = true
		c := map[string]any{"name": name, "declared": f != nil, "id": name == s.IDField}
		if f != nil {
			c["type"], c["required"], c["format"], c["enum"], c["description"] = f.Type, f.Required, f.Format, f.Enum, f.Description
			if f.Min != nil {
				c["min"] = *f.Min
			}
			if f.Max != nil {
				c["max"] = *f.Max
			}
			if f.Pattern != "" {
				c["pattern"] = f.Pattern
			}
		}
		out = append(out, c)
	}
	add(s.IDField, s.Fields[s.IDField])
	for _, k := range sortedKeys(s.Fields) {
		add(k, s.Fields[k])
	}
	extra := map[string]bool{}
	for i, row := range s.Data {
		if i >= 2000 {
			break
		}
		for k := range row {
			if !seen[k] {
				extra[k] = true
			}
		}
	}
	for _, k := range sortedSet(extra) {
		add(k, nil)
	}
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// readSpec decodes an API file body sent by the console.
func readSpec(r *http.Request) (*APISpec, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, &httpErr{413, "body too large"}
	}
	s, err := decodeSpec(b)
	if err != nil {
		return nil, &httpErr{400, "not a valid API file: " + err.Error()}
	}
	return s, nil
}

func (a *Admin) validateAPI(w http.ResponseWriter, r *http.Request) {
	s, err := readSpec(r)
	if err == nil {
		if cur := a.app.store.Get(s.key()); cur != nil && r.URL.Query().Get("existing") == s.key() {
			restoreSecrets(s, cur)
			if s.Data == nil {
				s.Data = cur.clone().Data
			}
		}
		if err = hasMask(s); err == nil {
			err = s.normalize()
		}
	}
	if err != nil {
		writeJSON(w, 200, map[string]any{"valid": false, "error": strings.TrimPrefix(err.Error(), "invalid: ")})
		return
	}
	writeJSON(w, 200, map[string]any{"valid": true, "key": s.key(), "records": len(s.Data), "path": a.app.cfg.APIBasePath + s.basePath()})
}

// generateToken makes a random API token; only its hash goes into the file.
func (a *Admin) generateToken(w http.ResponseWriter, r *http.Request) {
	t := randomToken()
	sum := sha256.Sum256([]byte(t))
	writeJSON(w, 200, map[string]string{"token": t, "ref": "sha256:" + hex.EncodeToString(sum[:])})
}

// createAPI writes a new API file (?file=market/holidays.json picks its path).
func (a *Admin) createAPI(w http.ResponseWriter, r *http.Request) {
	s, err := readSpec(r)
	if err != nil {
		adminErr(w, err)
		return
	}
	if err := hasMask(s); err != nil {
		errJSON(w, 422, err.Error())
		return
	}
	n, err := a.app.store.Create(s, strings.TrimPrefix(r.URL.Query().Get("file"), apisSubdir+"/"))
	if err != nil {
		adminErr(w, err)
		return
	}
	a.app.afterWrite(r.Context(), n.key(), a.who(r).entry(n.key(), "", "api_create", nil, maskedContract(n)))
	slog.Info("api created", "kind", "config", "api", n.key(), "file", apisSubdir+"/"+n.rel, "client", principalOf(r).Name)
	writeJSON(w, 201, a.summary(n, nil))
}

// putAPI replaces the contract; the records stay as they are.
func (a *Admin) putAPI(w http.ResponseWriter, r *http.Request) {
	cur := a.spec(w, r)
	if cur == nil {
		return
	}
	s, err := readSpec(r)
	if err != nil {
		adminErr(w, err)
		return
	}
	if s.Data != nil {
		errJSON(w, 400, `send the contract without "data": records are changed in the Records tab, by import, or through the API`)
		return
	}
	restoreSecrets(s, cur)
	if err := hasMask(s); err != nil {
		errJSON(w, 422, err.Error())
		return
	}
	before := maskedContract(cur)
	n, err := a.app.store.SaveContract(cur.key(), s)
	if err != nil {
		adminErr(w, err)
		return
	}
	if n.key() != cur.key() {
		a.app.cache.Forget(cur.key())
	}
	a.app.afterWrite(r.Context(), n.key(), a.who(r).entry(n.key(), "", "api_update", before, maskedContract(n)))
	writeJSON(w, 200, a.summary(n, nil))
}

func (a *Admin) deleteAPI(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("v") + "/" + r.PathValue("n")
	s, moved, err := a.app.store.Delete(key)
	if err != nil {
		adminErr(w, err)
		return
	}
	a.app.cache.Forget(key)
	a.app.audit.Record(r.Context(), a.who(r).entry(key, "", "api_delete", maskedContract(s), nil))
	slog.Info("api deleted", "kind", "config", "api", key, "moved_to", moved, "client", principalOf(r).Name)
	writeJSON(w, 200, map[string]any{"deleted": key, "moved_to": moved})
}

func (a *Admin) purgeCache(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	n := a.app.cache.Purge(s.key())
	slog.Info("cache purged", "kind", "admin", "api", s.key(), "entries", n, "client", principalOf(r).Name)
	writeJSON(w, 200, map[string]any{"purged": n, "cache": a.app.cache.Stats(s)})
}

// ---------- records ----------

func (a *Admin) listRecords(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	rq, err := parseConsoleQuery(r.URL.Query(), maxPageSize)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		rq.offset, rq.limit = 0, len(s.Data)+1
		all, _ := rq.run(s.Data, s.IDField)
		a.exportRecordsCSV(w, r, s, all)
		return
	}
	all, page := rq.run(s.Data, s.IDField)
	sorts := []string{}
	for _, k := range rq.sorts {
		d := "asc"
		if k.desc {
			d = "desc"
		}
		sorts = append(sorts, k.field+":"+d)
	}
	writeJSON(w, 200, map[string]any{"items": page, "total": len(all), "offset": rq.offset, "limit": rq.limit, "sort": sorts, "etag": s.etag})
}

func (a *Admin) getRecord(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	row, ok := s.record(r.PathValue("id"))
	if !ok {
		errJSON(w, 404, "record not found")
		return
	}
	writeJSON(w, 200, map[string]any{"data": row, "etag": recordETag(row)})
}

func (a *Admin) createRecord(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	body, err := decodeObject(r)
	if err != nil {
		adminErr(w, err)
		return
	}
	row, _, err := a.app.createRecord(r.Context(), s.key(), body, a.who(r))
	if err != nil {
		adminErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{"data": row, "etag": recordETag(row)})
}

func (a *Admin) putRecord(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	body, err := decodeObject(r)
	if err != nil {
		adminErr(w, err)
		return
	}
	row, created, _, err := a.app.replaceRecord(r.Context(), s.key(), r.PathValue("id"), body, r.Header.Get("If-Match"), a.who(r))
	if err != nil {
		adminErr(w, err)
		return
	}
	code := 200
	if created {
		code = 201
	}
	writeJSON(w, code, map[string]any{"data": row, "etag": recordETag(row)})
}

func (a *Admin) patchRecord(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	body, err := decodeObject(r)
	if err != nil {
		adminErr(w, err)
		return
	}
	row, _, err := a.app.patchRecord(r.Context(), s.key(), r.PathValue("id"), body, r.Header.Get("If-Match"), a.who(r))
	if err != nil {
		adminErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"data": row, "etag": recordETag(row)})
}

func (a *Admin) deleteRecord(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	if err := a.app.deleteRecord(r.Context(), s.key(), r.PathValue("id"), r.Header.Get("If-Match"), a.who(r)); err != nil {
		adminErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// importRecords takes a JSON array of records (?mode=upsert|replace).
func (a *Admin) importRecords(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	var rows []map[string]any
	if err := dec.Decode(&rows); err != nil {
		errJSON(w, 400, "body must be a JSON array of records: "+err.Error())
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "upsert"
	}
	res, err := a.app.importRecords(r.Context(), s.key(), rows, mode, a.who(r))
	if err != nil {
		adminErr(w, err)
		return
	}
	writeJSON(w, 200, res)
}

// exportRecords downloads every record as a JSON array (the "data" of the file).
func (a *Admin) exportRecords(w http.ResponseWriter, r *http.Request) {
	s := a.spec(w, r)
	if s == nil {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(s.Data)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+s.Version+"-"+s.Name+"-"+time.Now().Format("20060102-150405")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(buf.Bytes())
}

// ---------- audit ----------

func (a *Admin) auditQuery(r *http.Request) (AuditQuery, error) {
	q := r.URL.Query()
	lp, err := auditListSpec.parse(q)
	if err != nil {
		return AuditQuery{}, err
	}
	return AuditQuery{API: q.Get("api"), RecordID: q.Get("record_id"), Text: q.Get("q"),
		Limit: clampLimit(queryInt(r, "limit", 50), 50), List: lp}, nil
}

func (a *Admin) listAudit(w http.ResponseWriter, r *http.Request) {
	aq, err := a.auditQuery(r)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	if r.URL.Query().Get("format") == "csv" {
		aq.List.Offset = 0
		a.exportAudit(w, r, aq)
		return
	}
	items, err := a.app.audit.List(r.Context(), aq)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	total, err := a.app.audit.Count(r.Context(), aq)
	if err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, pageJSON(items, 0, total, aq.List, aq.Limit))
}

func (a *Admin) auditEntry(w http.ResponseWriter, r *http.Request) (AuditEntry, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, 400, "bad id")
		return AuditEntry{}, false
	}
	e, err := a.app.audit.Get(r.Context(), id)
	if err != nil {
		errJSON(w, 404, "not found")
		return AuditEntry{}, false
	}
	return e, true
}

func (a *Admin) getAudit(w http.ResponseWriter, r *http.Request) {
	if e, ok := a.auditEntry(w, r); ok {
		writeJSON(w, 200, e)
	}
}

func (a *Admin) revertAudit(w http.ResponseWriter, r *http.Request) {
	e, ok := a.auditEntry(w, r)
	if !ok {
		return
	}
	re, err := a.app.revert(r.Context(), e, a.who(r))
	if err != nil {
		adminErr(w, err)
		return
	}
	writeJSON(w, 200, re)
}

// ---------- access allowlists ----------

type accessView struct {
	Admin        []string     `json:"admin"`
	Catalog      []string     `json:"catalog"`
	APIs         []apiSummary `json:"apis"`
	Default      []string     `json:"default"`
	ClientIP     string       `json:"client_ip"`
	AdminLocked  bool         `json:"admin_locked"`
	TrustedProxy []string     `json:"trusted_proxies"`
	Notes        []DeployNote `json:"notes"`
	PublicCatlg  bool         `json:"public_catalog"`
}

func (a *Admin) getAccess(w http.ResponseWriter, r *http.Request) {
	ac := a.app.store.Access()
	apis := []apiSummary{}
	for _, s := range a.app.store.All() {
		apis = append(apis, a.summary(s, nil))
	}
	writeJSON(w, 200, accessView{Admin: ac.Admin.Allow, Catalog: ac.Catalog.Allow, APIs: apis, Default: defaultAllow,
		ClientIP: ClientIP(r, a.app.cfg.trusted).String(), AdminLocked: true, TrustedProxy: orEmpty(a.app.cfg.TrustedProxies),
		Notes: orEmpty(accessNotes(a.app.cfg, a.app.store.snap.Load(), a.app.env)), PublicCatlg: a.app.cfg.PublicCatalog})
}

// putAccess changes the catalog allowlist. The admin list can only be
// changed in the file, so an editor can neither lock everyone out of the
// console nor open it to the internet over the API. Each API carries its
// own allowed_ips and is edited with its contract.
func (a *Admin) putAccess(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Admin   *[]string `json:"admin"`
		Catalog *[]string `json:"catalog"`
	}
	if err := decodeStrict(r.Body, &in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	cur := a.app.store.Access()
	if in.Admin != nil && !slices.Equal(*in.Admin, cur.Admin.Allow) {
		errJSON(w, 403, "the admin allowlist is file-only: edit access.json in the dynamic config directory")
		return
	}
	next := &AccessConfig{Admin: AccessList{Allow: slices.Clone(cur.Admin.Allow)}, Catalog: AccessList{Allow: slices.Clone(cur.Catalog.Allow)}}
	if in.Catalog != nil {
		next.Catalog.Allow = orEmpty(*in.Catalog)
	}
	if err := a.app.store.SaveAccess(next); err != nil {
		adminErr(w, err)
		return
	}
	a.getAccess(w, r)
}

// testAccess answers "would this address be let in?" for the console, the
// catalog and every API.
func (a *Admin) testAccess(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IP string `json:"ip"`
	}
	if err := decodeStrict(r.Body, &in); err != nil {
		errJSON(w, 400, err.Error())
		return
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(in.IP))
	if err != nil {
		errJSON(w, 400, "not an IP address")
		return
	}
	type verdict struct {
		API     string `json:"api,omitempty"`
		Allowed bool   `json:"allowed"`
		Rule    string `json:"rule,omitempty"`
	}
	ac := a.app.store.Access()
	out := map[string]any{"ip": ip.Unmap().String()}
	rule, ok := ac.Admin.list.Match(ip)
	out["admin"] = verdict{Allowed: ok, Rule: rule}
	rule, ok = ac.Catalog.list.Match(ip)
	out["catalog"] = verdict{Allowed: ok, Rule: rule}
	apis := []verdict{}
	for _, s := range a.app.store.All() {
		rule, ok := s.allowsIP(ip)
		apis = append(apis, verdict{API: s.key(), Allowed: ok, Rule: rule})
	}
	out["apis"] = apis
	writeJSON(w, 200, out)
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
