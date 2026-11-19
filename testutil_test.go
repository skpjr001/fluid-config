package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	editorToken = "editor-token-0123456789abcdef"
	viewerToken = "viewer-token-0123456789abcdef"
	apiToken    = "api-token-0123456789"
)

type testApp struct {
	*App
	t     *testing.T
	dir   string // dynamic dir
	api   *httptest.Server
	admin *httptest.Server
}

// holidays is a small writable API used by most tests.
func holidays() map[string]any {
	return map[string]any{
		"name": "holidays", "version": "v1", "methods": []string{"GET", "POST", "PUT", "PATCH", "DELETE"},
		"auth":   map[string]any{"type": "bearer", "tokens": []string{apiToken}, "read_public": true},
		"fields": map[string]any{"date": map[string]any{"type": "string", "format": "date", "required": true}, "exchange": map[string]any{"type": "string", "enum": []string{"NSE", "BSE"}}},
		"data": []map[string]any{
			{"id": 1, "date": "2026-01-26", "exchange": "NSE"},
			{"id": 2, "date": "2026-03-03", "exchange": "BSE"},
		},
	}
}

// newTestApp runs both handlers on httptest servers with a fresh dynamic dir
// and database. Background loops (watch, housekeeping) are not running:
// tests call store.Reload themselves.
func newTestApp(t *testing.T, mutate func(*StaticConfig), files map[string]any) *testApp {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultStatic()
	cfg.DynamicDir = filepath.Join(dir, "dynamic")
	cfg.Database.Path = filepath.Join(dir, "data", "test.db")
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	ta := &testApp{t: t, dir: cfg.DynamicDir}
	ta.writeJSON("admin.json", AdminConfig{Tokens: []AdminToken{
		{Name: "ed", Token: editorToken, Role: RoleEditor},
		{Name: "vi", Token: viewerToken, Role: RoleViewer},
	}})
	if files == nil {
		files = map[string]any{"apis/holidays.json": holidays()}
	}
	for rel, v := range files {
		ta.writeJSON(rel, v)
	}
	app, err := NewApp(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ta.App = app
	ta.api = httptest.NewServer(app.apiHandler())
	ta.admin = httptest.NewServer(app.adminHandler())
	t.Cleanup(func() {
		ta.api.Close()
		ta.admin.Close()
		app.sink.Close()
		app.db.Close()
	})
	return ta
}

func (ta *testApp) writeJSON(rel string, v any) {
	ta.t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		ta.t.Fatal(err)
	}
	p := filepath.Join(ta.dir, rel)
	os.MkdirAll(filepath.Dir(p), 0o750)
	if err := os.WriteFile(p, b, 0o640); err != nil {
		ta.t.Fatal(err)
	}
	// a distinct mtime, so the reload fingerprint and the hand-edit check see the change
	future := time.Now().Add(time.Duration(len(b)%997+1) * time.Millisecond)
	os.Chtimes(p, future, future)
}

func (ta *testApp) reload() {
	ta.t.Helper()
	if err := ta.store.Reload(true); err != nil {
		ta.t.Fatal(err)
	}
}

type resp struct {
	code   int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.code, r.body)
	}
	return m
}

func do(t *testing.T, method, url string, body any, headers ...string) resp {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, b}
}

// call hits the public API listener.
func (ta *testApp) call(method, path string, body any, headers ...string) resp {
	ta.t.Helper()
	return do(ta.t, method, ta.api.URL+path, body, headers...)
}

// console hits the admin listener with a token.
func (ta *testApp) console(method, path, token string, body any, headers ...string) resp {
	ta.t.Helper()
	return do(ta.t, method, ta.admin.URL+path, body, append([]string{"Authorization", "Bearer " + token}, headers...)...)
}

func bearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

// fileData reads the records back from an API file on disk.
func (ta *testApp) fileData(rel string) []map[string]any {
	ta.t.Helper()
	b, err := os.ReadFile(filepath.Join(ta.dir, rel))
	if err != nil {
		ta.t.Fatal(err)
	}
	var f struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		ta.t.Fatal(err)
	}
	return f.Data
}

func (ta *testApp) auditRows(api string) []AuditEntry {
	ta.t.Helper()
	es, err := ta.audit.List(context.Background(), AuditQuery{API: api, Limit: 500})
	if err != nil {
		ta.t.Fatal(err)
	}
	return es
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
