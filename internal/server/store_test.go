package server

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestHandEditsAreAuditedAndPurgeCache(t *testing.T) {
	spec := holidays()
	spec["cache"] = map[string]any{"enabled": true}
	ta := newTestApp(t, nil, map[string]any{"apis/holidays.json": spec})
	ta.call("GET", "/v1/holidays", nil)
	if ta.cache.Stats(ta.store.Get("v1/holidays")).Entries != 1 {
		t.Fatal("expected one cached response")
	}
	// edit the file by hand: change one record, delete one, add one
	spec["data"] = []map[string]any{{"id": 1, "date": "2026-01-27", "exchange": "NSE"}, {"id": 5, "date": "2026-05-01"}}
	spec["description"] = "edited by hand"
	ta.writeJSON("apis/holidays.json", spec)
	ta.reload()
	es := ta.auditRows("v1/holidays")
	got := map[string]string{}
	for _, e := range es {
		if e.Via != "file" || e.Actor != "file" {
			t.Fatalf("hand edit attributed to %s/%s", e.Via, e.Actor)
		}
		got[e.Action+":"+e.RecordID] = string(e.After)
	}
	for _, k := range []string{"replace:1", "delete:2", "create:5", "api_update:"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %s in %v", k, got)
		}
	}
	if ta.cache.Stats(ta.store.Get("v1/holidays")).Entries != 0 {
		t.Fatal("a hand edit must drop cached responses")
	}
	// a write made through fluid-config is not reported again on reload
	n := len(es)
	ta.call("PATCH", "/v1/holidays/1", map[string]any{"exchange": "BSE"}, bearer(apiToken)...)
	ta.reload()
	if len(ta.auditRows("v1/holidays")) != n+1 {
		t.Fatal("own writes must appear once in the history")
	}
}

func TestWriteNeverOverwritesAHandEdit(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	spec := holidays()
	spec["data"] = append(spec["data"].([]map[string]any), map[string]any{"id": 7, "date": "2026-07-07"})
	ta.writeJSON("apis/holidays.json", spec) // not reloaded yet
	if r := ta.call("PATCH", "/v1/holidays/1", map[string]any{"exchange": "BSE"}, bearer(apiToken)...); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	ids := []string{}
	for _, d := range ta.fileData("apis/holidays.json") {
		ids = append(ids, toString(d["id"])+toString(d["exchange"]))
	}
	if strings.Join(ids, ",") != "1BSE,2BSE,7" {
		t.Fatalf("the hand-added record must survive the write: %v", ids)
	}
}

func TestBrokenFileKeepsLastGoodVersion(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	os.WriteFile(filepath.Join(ta.dir, "apis/holidays.json"), []byte(`{"name": "holidays", "version": "v1", "data": [{"id": 1}], "typo": 1}`), 0o640)
	ta.reload()
	if r := ta.call("GET", "/v1/holidays", nil); r.code != 200 || len(r.json(t)["data"].([]any)) != 2 {
		t.Fatalf("previous version must still be served: %d %s", r.code, r.body)
	}
	if e := ta.store.Errors()["apis/holidays.json"]; !strings.Contains(e, "typo") {
		t.Fatalf("error not reported: %v", ta.store.Errors())
	}
	// writes are refused until the file is fixed: they would discard the hand edit
	if r := ta.call("PATCH", "/v1/holidays/1", map[string]any{"exchange": "BSE"}, bearer(apiToken)...); r.code != 409 {
		t.Fatalf("write to an invalid file: %d %s", r.code, r.body)
	}
	if r := ta.console("GET", "/api/apis", viewerToken, nil); !strings.Contains(string(r.body), "typo") {
		t.Fatalf("console shows the file error: %s", r.body)
	}
}

func TestDuplicateAPIsAndReadOnlyFiles(t *testing.T) {
	files := map[string]any{"apis/a.json": holidays(), "apis/b/holidays.json": holidays()}
	ta := newTestApp(t, nil, files)
	if len(ta.store.All()) != 1 || !strings.Contains(ta.store.Errors()["apis/b/holidays.json"], "duplicate") {
		t.Fatalf("duplicate: %v", ta.store.Errors())
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permissions")
	}
	os.Chmod(filepath.Join(ta.dir, "apis"), 0o550)
	defer os.Chmod(filepath.Join(ta.dir, "apis"), 0o750)
	r := ta.call("POST", "/v1/holidays", map[string]any{"date": "2026-01-01"}, bearer(apiToken)...)
	if r.code != 409 || !strings.Contains(string(r.body), "read-only") {
		t.Fatalf("read-only dir: %d %s", r.code, r.body)
	}
}

func TestSpecValidation(t *testing.T) {
	bad := map[string]map[string]any{
		"name":      {"name": "Bad Name", "version": "v1"},
		"version":   {"name": "x", "version": "1"},
		"method":    {"name": "x", "version": "v1", "methods": []string{"TRACE"}},
		"type":      {"name": "x", "version": "v1", "fields": map[string]any{"a": map[string]any{"type": "text"}}},
		"pattern":   {"name": "x", "version": "v1", "fields": map[string]any{"a": map[string]any{"type": "string", "pattern": "("}}},
		"ip":        {"name": "x", "version": "v1", "allowed_ips": []string{"10.0.0.300"}},
		"tokens":    {"name": "x", "version": "v1", "auth": map[string]any{"type": "bearer"}},
		"env":       {"name": "x", "version": "v1", "auth": map[string]any{"type": "bearer", "tokens": []string{"env:FC_UNSET_VAR_X"}}},
		"data":      {"name": "x", "version": "v1", "fields": map[string]any{"a": map[string]any{"type": "integer"}}, "data": []map[string]any{{"id": 1, "a": 1.5}}},
		"dup":       {"name": "x", "version": "v1", "data": []map[string]any{{"id": 1}, {"id": "1"}}},
		"cache":     {"name": "x", "version": "v1", "cache": map[string]any{"enabled": true, "ttl_seconds": -1}},
		"rate":      {"name": "x", "version": "v1", "rate_limit": map[string]any{"requests_per_minute": 5}},
		"cors":      {"name": "x", "version": "v1", "cors_origins": []string{"app.example.com"}},
		"none+toks": {"name": "x", "version": "v1", "auth": map[string]any{"type": "none", "tokens": []string{"x"}}},
	}
	for name, v := range bad {
		ta := &testApp{t: t, dir: t.TempDir()}
		ta.writeJSON("x.json", v)
		if _, err := parseSpecFile(filepath.Join(ta.dir, "x.json")); err == nil {
			t.Errorf("%s: should be invalid", name)
		}
	}
	f := &FieldSpec{Type: "string", Format: "email"}
	if f.check("a@b") != nil || f.check("nope") == nil {
		t.Error("email format")
	}
}

func TestMergePatchIsDeep(t *testing.T) {
	row := map[string]any{"a": map[string]any{"x": 1, "y": 2}, "b": 1}
	out := mergePatch(row, map[string]any{"a": map[string]any{"y": nil, "z": 3}, "b": nil, "c": []any{1}})
	if toString(out) != `{"a":{"x":1,"z":3},"c":[1]}` {
		t.Fatalf("%s", toString(out))
	}
	if toString(row) != `{"a":{"x":1,"y":2},"b":1}` {
		t.Fatal("the original row must not change")
	}
}

func TestConcurrentWritesLoseNothing(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	const n = 40
	errs := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			r := ta.call("POST", "/v1/holidays", map[string]any{"date": "2026-06-01"}, bearer(apiToken)...)
			errs <- r.code
		}(i)
	}
	for i := 0; i < n; i++ {
		if c := <-errs; c != 201 {
			t.Fatalf("write %d", c)
		}
	}
	if got := len(ta.fileData("apis/holidays.json")); got != n+2 {
		t.Fatalf("file holds %d records, want %d", got, n+2)
	}
	if got := len(ta.auditRows("v1/holidays")); got != n {
		t.Fatalf("history holds %d rows, want %d", got, n)
	}
}
