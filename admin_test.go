package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleRoles(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	if r := ta.console("GET", "/api/apis", "wrong-token-0123456789", nil); r.code != 401 {
		t.Fatalf("bad token: %d", r.code)
	}
	if r := ta.console("GET", "/api/whoami", viewerToken, nil); !strings.Contains(string(r.body), `"viewer"`) {
		t.Fatalf("whoami: %s", r.body)
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/apis/v1/holidays/records"}, {"PATCH", "/api/apis/v1/holidays/records/1"}, {"DELETE", "/api/apis/v1/holidays/records/1"},
		{"PUT", "/api/apis/v1/holidays"}, {"DELETE", "/api/apis/v1/holidays"}, {"POST", "/api/apis"}, {"POST", "/api/apis/v1/holidays/import"},
		{"POST", "/api/apis/v1/holidays/cache/purge"}, {"POST", "/api/audit/1/revert"}, {"PUT", "/api/access"}, {"POST", "/api/tokens/generate"},
	} {
		if r := ta.console(c.method, c.path, viewerToken, map[string]any{}); r.code != 403 {
			t.Errorf("viewer %s %s: %d", c.method, c.path, r.code)
		}
	}
	for _, p := range []string{"/api/status", "/api/apis", "/api/apis/v1/holidays", "/api/apis/v1/holidays/records", "/api/audit", "/api/logs", "/api/access", "/metrics", "/api/openapi.json"} {
		if r := ta.console("GET", p, viewerToken, nil); r.code != 200 {
			t.Errorf("viewer GET %s: %d %s", p, r.code, r.body)
		}
	}
}

func TestConsoleRecordsQueryAndCSV(t *testing.T) {
	spec := holidays()
	var data []map[string]any
	for i := 1; i <= 30; i++ {
		ex := "NSE"
		if i%3 == 0 {
			ex = "BSE"
		}
		data = append(data, map[string]any{"id": i, "date": fmt.Sprintf("2026-01-%02d", i), "exchange": ex, "note": "=cmd"})
	}
	spec["data"] = data
	ta := newTestApp(t, nil, map[string]any{"apis/h.json": spec})
	r := ta.console("GET", "/api/apis/v1/holidays/records?f.exchange==BSE&sort=date:desc&limit=4&offset=2", viewerToken, nil)
	m := r.json(t)
	items := m["items"].([]any)
	if m["total"].(float64) != 10 || len(items) != 4 || items[0].(map[string]any)["date"] != "2026-01-24" {
		t.Fatalf("query: %s", r.body)
	}
	r = ta.console("GET", "/api/apis/v1/holidays/records?f.date=2026-01-05..2026-01-07&f.id=!6", viewerToken, nil)
	if r.json(t)["total"].(float64) != 2 {
		t.Fatalf("range and exclusion: %s", r.body)
	}
	r = ta.console("GET", "/api/apis/v1/holidays/records?format=csv&f.exchange==BSE&tz=Asia/Kolkata", viewerToken, nil)
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(r.body), "\xef\xbb\xbf"))).ReadAll()
	if err != nil || len(recs) != 11 || recs[0][0] != "id" || recs[1][3] != "'=cmd" {
		t.Fatalf("csv: %v %v", recs, err)
	}
	d := ta.console("GET", "/api/apis/v1/holidays", viewerToken, nil).json(t)
	cols := d["columns"].([]any)
	if len(cols) != 4 || cols[3].(map[string]any)["declared"] != false {
		t.Fatalf("columns: %v", cols)
	}
}

func TestConsoleRecordWritesAndRevert(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	g := ta.console("GET", "/api/apis/v1/holidays/records/1", editorToken, nil).json(t)
	etag := g["etag"].(string)
	if r := ta.console("PUT", "/api/apis/v1/holidays/records/1", editorToken, map[string]any{"date": "2026-01-27", "exchange": "NSE"}, "If-Match", etag); r.code != 200 {
		t.Fatalf("put: %d %s", r.code, r.body)
	}
	if r := ta.console("PUT", "/api/apis/v1/holidays/records/1", editorToken, map[string]any{"date": "2026-01-28"}, "If-Match", etag); r.code != 412 {
		t.Fatalf("stale etag: %d", r.code)
	}
	// the console may write even to an API that is read-only over HTTP
	es := ta.auditRows("v1/holidays")
	if len(es) != 1 || es[0].Actor != "ed" || es[0].Via != "console" || !strings.Contains(string(es[0].Before), "2026-01-26") {
		t.Fatalf("audit: %+v", es)
	}
	if r := ta.console("POST", fmt.Sprintf("/api/audit/%d/revert", es[0].ID), editorToken, nil); r.code != 200 {
		t.Fatalf("revert: %d %s", r.code, r.body)
	}
	if d := ta.fileData("apis/holidays.json"); d[0]["date"] != "2026-01-26" {
		t.Fatalf("revert did not restore: %v", d[0])
	}
	// revert a delete: the record comes back
	ta.console("DELETE", "/api/apis/v1/holidays/records/2", editorToken, nil)
	del := ta.auditRows("v1/holidays")[0]
	if del.Action != "delete" {
		t.Fatalf("%+v", del)
	}
	ta.console("POST", fmt.Sprintf("/api/audit/%d/revert", del.ID), editorToken, nil)
	if d := ta.fileData("apis/holidays.json"); len(d) != 2 {
		t.Fatalf("deleted record not restored: %v", d)
	}
	// and a create: the record goes
	c := ta.console("POST", "/api/apis/v1/holidays/records", editorToken, map[string]any{"date": "2026-09-09"})
	if c.code != 201 {
		t.Fatalf("create: %d %s", c.code, c.body)
	}
	cr := ta.auditRows("v1/holidays")[0]
	ta.console("POST", fmt.Sprintf("/api/audit/%d/revert", cr.ID), editorToken, nil)
	if r := ta.console("POST", fmt.Sprintf("/api/audit/%d/revert", cr.ID), editorToken, nil); r.code != 409 {
		t.Fatalf("second revert of a create: %d", r.code)
	}
	if d := ta.fileData("apis/holidays.json"); len(d) != 2 {
		t.Fatalf("created record not removed: %v", d)
	}
}

func TestImport(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	rows := []map[string]any{{"id": 2, "date": "2026-03-04", "exchange": "BSE"}, {"id": 3, "date": "2026-04-14"}}
	r := ta.console("POST", "/api/apis/v1/holidays/import?mode=upsert", editorToken, rows)
	if r.code != 200 || !strings.Contains(string(r.body), `"created":1,"replaced":1,"deleted":0,"total":3`) {
		t.Fatalf("upsert: %d %s", r.code, r.body)
	}
	// one invalid row: nothing is written
	bad := []map[string]any{{"id": 9, "date": "2026-01-01"}, {"id": 10, "date": "soon"}}
	if r := ta.console("POST", "/api/apis/v1/holidays/import?mode=replace", editorToken, bad); r.code != 422 || !strings.Contains(string(r.body), "row 2") {
		t.Fatalf("invalid import: %d %s", r.code, r.body)
	}
	if len(ta.fileData("apis/holidays.json")) != 3 {
		t.Fatal("a failed import must not change the file")
	}
	r = ta.console("POST", "/api/apis/v1/holidays/import?mode=replace", editorToken, []map[string]any{{"id": 1, "date": "2026-01-26", "exchange": "NSE"}})
	if !strings.Contains(string(r.body), `"deleted":2,"total":1`) || len(ta.fileData("apis/holidays.json")) != 1 {
		t.Fatalf("replace: %s", r.body)
	}
	if n := len(ta.auditRows("v1/holidays")); n != 4 {
		t.Fatalf("each imported change is in the history: %d rows", n)
	}
	exp := ta.console("GET", "/api/apis/v1/holidays/export", viewerToken, nil)
	if exp.code != 200 || !strings.Contains(exp.header.Get("Content-Disposition"), "v1-holidays") || !strings.HasPrefix(strings.TrimSpace(string(exp.body)), "[") {
		t.Fatalf("export: %d %v", exp.code, exp.header)
	}
}

func TestContractEditingKeepsSecretsAndValidatesData(t *testing.T) {
	spec := holidays()
	spec["auth"] = map[string]any{"type": "bearer", "tokens": []string{"literal-secret-token", "ci=env:FC_CI"}, "read_public": true}
	t.Setenv("FC_CI", "ci-secret-from-env")
	ta := newTestApp(t, nil, map[string]any{"apis/holidays.json": spec})
	d := ta.console("GET", "/api/apis/v1/holidays", viewerToken, nil)
	if strings.Contains(string(d.body), "literal-secret-token") || !strings.Contains(string(d.body), "ci=env:FC_CI") {
		t.Fatalf("contract view must mask literals only: %s", d.body)
	}
	c := d.json(t)["contract"].(map[string]any)
	c["description"] = "changed in the console"
	if r := ta.console("PUT", "/api/apis/v1/holidays", editorToken, c); r.code != 200 {
		t.Fatalf("put contract: %d %s", r.code, r.body)
	}
	b, _ := os.ReadFile(filepath.Join(ta.dir, "apis/holidays.json"))
	if !strings.Contains(string(b), "literal-secret-token") || !strings.Contains(string(b), "changed in the console") || !strings.Contains(string(b), "2026-03-03") {
		t.Fatalf("file after contract edit:\n%s", b)
	}
	// a contract the existing records do not satisfy is refused
	c["fields"] = map[string]any{"reason": map[string]any{"type": "string", "required": true}}
	if r := ta.console("PUT", "/api/apis/v1/holidays", editorToken, c); r.code != 422 || !strings.Contains(string(r.body), "reason") {
		t.Fatalf("contract vs data: %d %s", r.code, r.body)
	}
	c["data"] = []any{}
	if r := ta.console("PUT", "/api/apis/v1/holidays", editorToken, c); r.code != 400 {
		t.Fatalf("data in a contract edit: %d", r.code)
	}
	es := ta.auditRows("v1/holidays")
	if len(es) != 1 || es[0].Action != "api_update" || strings.Contains(string(es[0].After), "literal-secret-token") {
		t.Fatalf("contract audit must not hold literal secrets: %+v", es)
	}
	// rename the version: the key moves
	delete(c, "data")
	c["fields"] = nil
	c["version"] = "v2"
	if r := ta.console("PUT", "/api/apis/v1/holidays", editorToken, c); r.code != 200 || ta.store.Get("v2/holidays") == nil || ta.store.Get("v1/holidays") != nil {
		t.Fatalf("rename: %d %s", r.code, r.body)
	}
}

func TestCreateAndDeleteAPIFiles(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	spec := map[string]any{"name": "flags", "version": "v1", "id_field": "key", "methods": []string{"GET", "PUT"},
		"auth": map[string]any{"type": "api_key", "tokens": []string{"x=sha256:" + sha256hex("k-123456")}}, "data": []map[string]any{{"key": "dark_mode"}}}
	if r := ta.console("POST", "/api/validate", viewerToken, spec); !strings.Contains(string(r.body), `"valid":true`) {
		t.Fatalf("validate: %s", r.body)
	}
	for _, bad := range []string{"../x.json", "/etc/x.json", "a/.hidden.json", "UPPER.json", "x.txt"} {
		if r := ta.console("POST", "/api/apis?file="+bad, editorToken, spec); r.code != 422 {
			t.Errorf("file %q: %d %s", bad, r.code, r.body)
		}
	}
	r := ta.console("POST", "/api/apis?file=apis/flags/main.json", editorToken, spec)
	if r.code != 201 {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	b, err := os.ReadFile(filepath.Join(ta.dir, "apis/flags/main.json"))
	if err != nil || !strings.Contains(string(b), `"$schema": "`+SchemaBase+`api.schema.json"`) {
		t.Fatalf("file: %v\n%s", err, b)
	}
	if r := ta.call("PUT", "/v1/flags/dark_mode", map[string]any{"on": true}, "X-API-Key", "k-123456"); r.code != 200 {
		t.Fatalf("new API serves at once: %d %s", r.code, r.body)
	}
	if r := ta.console("POST", "/api/apis", editorToken, spec); r.code != 409 {
		t.Fatalf("duplicate api: %d", r.code)
	}
	r = ta.console("DELETE", "/api/apis/v1/flags", editorToken, nil)
	if r.code != 200 || !strings.Contains(string(r.body), "apis/.deleted/flags__main.json.") {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if r := ta.call("GET", "/v1/flags", nil); r.code != 404 {
		t.Fatalf("deleted api: %d", r.code)
	}
	ta.reload()
	if ta.store.Get("v1/flags") != nil {
		t.Fatal("files in .deleted must not load")
	}
	acts := []string{}
	for _, e := range ta.auditRows("v1/flags") {
		acts = append(acts, e.Action)
	}
	if strings.Join(acts, ",") != "api_delete,replace,api_create" {
		t.Fatalf("audit: %v", acts)
	}
	tok := ta.console("POST", "/api/tokens/generate", editorToken, nil).json(t)
	if len(tok["token"].(string)) != 64 || tok["ref"] != "sha256:"+sha256hex(tok["token"].(string)) {
		t.Fatalf("generated token: %v", tok)
	}
}

func TestAccessListsOverConsole(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	if r := ta.console("PUT", "/api/access", editorToken, map[string]any{"admin": []string{"*"}}); r.code != 403 {
		t.Fatalf("admin list is file-only: %d", r.code)
	}
	if r := ta.console("PUT", "/api/access", editorToken, map[string]any{"catalog": []string{"10.*"}}); r.code != 200 {
		t.Fatalf("catalog: %d %s", r.code, r.body)
	}
	r := ta.console("POST", "/api/access/test", viewerToken, map[string]any{"ip": "10.9.9.9"}).json(t)
	if !r["catalog"].(map[string]any)["allowed"].(bool) || r["apis"].([]any)[0].(map[string]any)["rule"] != "10.0.0.0/8" {
		t.Fatalf("test: %v", r)
	}
	// the console itself refuses addresses outside admin.allow
	ta.writeJSON("access.json", map[string]any{"admin": map[string]any{"allow": []string{"10.0.0.0/8"}}})
	ta.reload()
	if r := ta.console("GET", "/api/status", editorToken, nil); r.code != 403 {
		t.Fatalf("console allowlist: %d", r.code)
	}
	if r := do(t, "GET", ta.admin.URL+"/readyz", nil); r.code != 200 {
		t.Fatalf("local health checks pass the allowlist: %d", r.code)
	}
}
