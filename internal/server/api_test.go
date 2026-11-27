package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/skpjr001/fluid-config/internal/config"
)

func TestListFilterSortPage(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	r := ta.call("GET", "/v1/holidays?exchange=NSE", nil)
	m := r.json(t)
	if r.code != 200 || len(m["data"].([]any)) != 1 || m["meta"].(map[string]any)["total"].(float64) != 1 {
		t.Fatalf("filter: %d %s", r.code, r.body)
	}
	r = ta.call("GET", "/v1/holidays?sort=-date&limit=1&page=2&fields=date", nil)
	m = r.json(t)
	row := m["data"].([]any)[0].(map[string]any)
	if row["date"] != "2026-01-26" || row["exchange"] != nil || row["id"].(float64) != 1 {
		t.Fatalf("sort/page/fields: %s", r.body)
	}
	if r := ta.call("GET", "/v1/holidays?date[bogus]=1", nil); r.code != 400 {
		t.Fatalf("bad operator: %d", r.code)
	}
	for q, n := range map[string]int{"date[gte]=2026-02-01": 1, "exchange[in]=NSE,BSE": 2, "q=03-03": 1, "segment[exists]=false": 2, "exchange[ne]=NSE": 1} {
		r := ta.call("GET", "/v1/holidays?"+q, nil)
		if got := len(r.json(t)["data"].([]any)); got != n {
			t.Errorf("%s: %d rows, want %d", q, got, n)
		}
	}
	if r := ta.call("GET", "/v1/nope", nil); r.code != 404 {
		t.Fatalf("unknown api: %d", r.code)
	}
}

func TestWritesAuthValidationAndFile(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	if r := ta.call("POST", "/v1/holidays", map[string]any{"date": "2026-12-25"}); r.code != 401 || r.header.Get("WWW-Authenticate") == "" {
		t.Fatalf("write without token: %d", r.code)
	}
	r := ta.call("POST", "/v1/holidays", map[string]any{"date": "2026-12-25", "exchange": "NSE"}, bearer(apiToken)...)
	if r.code != 201 || r.header.Get("Location") != "/v1/holidays/3" || r.header.Get("ETag") == "" {
		t.Fatalf("create: %d %v %s", r.code, r.header, r.body)
	}
	if r := ta.call("POST", "/v1/holidays", map[string]any{"date": "christmas"}, bearer(apiToken)...); r.code != 422 || !strings.Contains(string(r.body), "date") {
		t.Fatalf("invalid date: %d %s", r.code, r.body)
	}
	if r := ta.call("POST", "/v1/holidays", map[string]any{"id": 1, "date": "2026-12-25"}, bearer(apiToken)...); r.code != 409 {
		t.Fatalf("duplicate id: %d", r.code)
	}
	if r := ta.call("POST", "/v1/holidays", "[1,2]", bearer(apiToken)...); r.code != 400 {
		t.Fatalf("not an object: %d", r.code)
	}
	if r := ta.call("PATCH", "/v1/holidays/3", map[string]any{"exchange": nil, "note": "x"}, bearer(apiToken)...); r.code != 200 || strings.Contains(string(r.body), "exchange") {
		t.Fatalf("merge patch: %d %s", r.code, r.body)
	}
	if r := ta.call("PUT", "/v1/holidays/9", map[string]any{"date": "2026-11-01"}, bearer(apiToken)...); r.code != 201 {
		t.Fatalf("put upsert: %d %s", r.code, r.body)
	}
	if r := ta.call("PUT", "/v1/holidays/9", map[string]any{"id": 8, "date": "2026-11-01"}, bearer(apiToken)...); r.code != 400 {
		t.Fatalf("put id mismatch: %d", r.code)
	}
	if r := ta.call("DELETE", "/v1/holidays/1", nil, bearer(apiToken)...); r.code != 204 {
		t.Fatalf("delete: %d", r.code)
	}
	if r := ta.call("DELETE", "/v1/holidays/1", nil, bearer(apiToken)...); r.code != 404 {
		t.Fatalf("delete twice: %d", r.code)
	}
	data := ta.fileData("apis/holidays.json")
	ids := []string{}
	for _, d := range data {
		ids = append(ids, toString(d["id"]))
	}
	if strings.Join(ids, ",") != "2,3,9" {
		t.Fatalf("file holds %v", ids)
	}
	// every change is in the history, with the credential's name
	es := ta.auditRows("v1/holidays")
	if len(es) != 4 || es[0].Action != "delete" || es[0].Actor != "token-1" || es[0].Via != "api" || es[0].RequestID == "" || es[0].After != nil {
		t.Fatalf("audit: %+v", es)
	}
}

func TestIfMatchAndETags(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	g := ta.call("GET", "/v1/holidays/1", nil)
	tag := g.header.Get("ETag")
	if g.code != 200 || tag == "" {
		t.Fatalf("get: %d %v", g.code, g.header)
	}
	if r := ta.call("GET", "/v1/holidays/1", nil, "If-None-Match", tag); r.code != 304 {
		t.Fatalf("If-None-Match: %d", r.code)
	}
	list := ta.call("GET", "/v1/holidays", nil)
	if r := ta.call("GET", "/v1/holidays", nil, "If-None-Match", list.header.Get("ETag")); r.code != 304 {
		t.Fatalf("list 304: %d", r.code)
	}
	if r := ta.call("PATCH", "/v1/holidays/1", map[string]any{"exchange": "BSE"}, append(bearer(apiToken), "If-Match", tag)...); r.code != 200 {
		t.Fatalf("matching If-Match: %d %s", r.code, r.body)
	}
	// the record changed, so the old tag no longer matches
	if r := ta.call("PATCH", "/v1/holidays/1", map[string]any{"exchange": "NSE"}, append(bearer(apiToken), "If-Match", tag)...); r.code != 412 {
		t.Fatalf("stale If-Match: %d", r.code)
	}
	if r := ta.call("DELETE", "/v1/holidays/1", nil, append(bearer(apiToken), "If-Match", tag)...); r.code != 412 {
		t.Fatalf("stale delete: %d", r.code)
	}
	if r := ta.call("PUT", "/v1/holidays/77", map[string]any{"date": "2026-01-01"}, append(bearer(apiToken), "If-Match", "*")...); r.code != 412 {
		t.Fatalf("If-Match * on a missing record: %d", r.code)
	}
}

func TestIPAllowlists(t *testing.T) {
	files := map[string]any{
		"apis/private.json": map[string]any{"name": "private", "version": "v1"},
		"apis/wild.json":    map[string]any{"name": "wild", "version": "v1", "allowed_ips": []string{"52.66.*.*"}, "cidrs": []string{"203.0.113.0/24"}},
		"apis/open.json":    map[string]any{"name": "open", "version": "v1", "allowed_ips": []string{"*"}},
	}
	ta := newTestApp(t, func(c *config.StaticConfig) { c.TrustedProxies = []string{"127.0.0.1"} }, files)
	from := func(path, ip string) int { return ta.call("GET", path, nil, "X-Forwarded-For", ip).code }
	cases := []struct {
		path, ip string
		code     int
	}{
		{"/v1/private", "10.1.2.3", 200}, {"/v1/private", "8.8.8.8", 403},
		{"/v1/wild", "52.66.200.1", 200}, {"/v1/wild", "52.67.0.1", 403}, {"/v1/wild", "203.0.113.9", 200}, {"/v1/wild", "10.0.0.1", 403},
		{"/v1/open", "8.8.8.8", 200},
	}
	for _, c := range cases {
		if got := from(c.path, c.ip); got != c.code {
			t.Errorf("%s from %s: %d, want %d", c.path, c.ip, got, c.code)
		}
	}
	// the refusal is logged with the real client address
	ta.sink.Close()
	rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"api"}, Status: "403"})
	if len(rows) != 3 || rows[0].SourceIP == "127.0.0.1" || rows[0].Route == "" {
		t.Fatalf("403 logs: %+v", rows)
	}
}

func TestRateLimitCORSAndMethods(t *testing.T) {
	spec := holidays()
	spec["rate_limit"] = map[string]any{"requests_per_minute": 60, "burst": 2}
	spec["cors_origins"] = []string{"https://app.example.com"}
	spec["methods"] = []string{"GET", "PATCH"}
	ta := newTestApp(t, nil, map[string]any{"apis/h.json": spec})
	pre := ta.call("OPTIONS", "/v1/holidays/1", nil, "Origin", "https://app.example.com", "Access-Control-Request-Method", "PATCH")
	if pre.code != 204 || pre.header.Get("Access-Control-Allow-Origin") != "https://app.example.com" || !strings.Contains(pre.header.Get("Access-Control-Allow-Headers"), "If-Match") {
		t.Fatalf("preflight: %d %v", pre.code, pre.header)
	}
	if r := ta.call("GET", "/v1/holidays", nil, "Origin", "https://evil.example"); r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("foreign origin must not get CORS headers")
	}
	if r := ta.call("POST", "/v1/holidays", map[string]any{}); r.code != 405 || r.header.Get("Allow") != "GET, PATCH" {
		t.Fatalf("method: %d %v", r.code, r.header)
	}
	codes := []int{}
	for i := 0; i < 3; i++ {
		codes = append(codes, ta.call("GET", "/v1/holidays", nil).code)
	}
	if fmt.Sprint(codes) != "[200 429 429]" && fmt.Sprint(codes) != "[429 429 429]" {
		t.Fatalf("rate limit codes %v (preflight and 405 also take tokens)", codes)
	}
}

func TestMaxRecordsAndHEAD(t *testing.T) {
	spec := holidays()
	spec["max_records"] = 2
	ta := newTestApp(t, nil, map[string]any{"apis/h.json": spec})
	if r := ta.call("POST", "/v1/holidays", map[string]any{"date": "2026-01-01"}, bearer(apiToken)...); r.code != 409 || !strings.Contains(string(r.body), "max_records") {
		t.Fatalf("max_records: %d %s", r.code, r.body)
	}
	if r := ta.call("HEAD", "/v1/holidays", nil); r.code != 200 || len(r.body) != 0 {
		t.Fatalf("HEAD: %d %q", r.code, r.body)
	}
}

func TestNamedAndHashedTokens(t *testing.T) {
	spec := holidays()
	spec["auth"] = map[string]any{"type": "api_key", "header": "X-Key", "tokens": []string{
		"ci-bot=sha256:" + sha256hex("ci-secret-value"), "deployer=env:FC_TEST_TOKEN", "a=literal=with=equals"}}
	t.Setenv("FC_TEST_TOKEN", "from-the-environment")
	ta := newTestApp(t, nil, map[string]any{"apis/h.json": spec})
	for tok, who := range map[string]string{"ci-secret-value": "ci-bot", "from-the-environment": "deployer", "a=literal=with=equals": "token-3"} {
		r := ta.call("PATCH", "/v1/holidays/1", map[string]any{"exchange": "BSE"}, "X-Key", tok)
		if r.code != 200 {
			t.Fatalf("%s: %d", who, r.code)
		}
		if es := ta.auditRows("v1/holidays"); es[0].Actor != who {
			t.Fatalf("actor %q, want %q", es[0].Actor, who)
		}
	}
	if r := ta.call("GET", "/v1/holidays", nil, "X-Key", "wrong"); r.code != 401 {
		t.Fatalf("wrong key: %d", r.code)
	}
}

func TestBasicAuth(t *testing.T) {
	spec := holidays()
	spec["auth"] = map[string]any{"type": "basic", "users": map[string]string{"ops": "s3cret-pass"}}
	ta := newTestApp(t, nil, map[string]any{"apis/h.json": spec})
	req, _ := http.NewRequest("GET", ta.api.URL+"/v1/holidays", nil)
	req.SetBasicAuth("ops", "s3cret-pass")
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("basic: %v %v", res.StatusCode, err)
	}
	res.Body.Close()
	if r := ta.call("GET", "/v1/holidays", nil); r.code != 401 || !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no credentials: %d", r.code)
	}
}

func TestCatalogNeedsToken(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	if r := ta.call("GET", "/_catalog", nil); r.code != 401 {
		t.Fatalf("catalog without token: %d", r.code)
	}
	r := ta.call("GET", "/openapi.json", nil, bearer(viewerToken)...)
	if r.code != 200 || !strings.Contains(string(r.body), `"/v1/holidays/{id}"`) || !strings.Contains(string(r.body), "If-Match") {
		t.Fatalf("openapi: %d %s", r.code, r.body)
	}
	open := newTestApp(t, func(c *config.StaticConfig) { c.PublicCatalog = true }, nil)
	if r := open.call("GET", "/_catalog", nil); r.code != 200 || !strings.Contains(string(r.body), `"records":2`) {
		t.Fatalf("public catalog: %d %s", r.code, r.body)
	}
	// the catalog allowlist still applies
	open.writeJSON("access.json", map[string]any{"catalog": map[string]any{"allow": []string{}}})
	open.reload()
	if r := open.call("GET", "/_catalog", nil); r.code != 403 {
		t.Fatalf("catalog allowlist: %d", r.code)
	}
}

func TestBasePathAndHealth(t *testing.T) {
	ta := newTestApp(t, func(c *config.StaticConfig) { c.APIBasePath = "/config" }, nil)
	for _, p := range []string{"/config/v1/holidays", "/v1/holidays", "/config/livez", "/readyz", "/healthz"} {
		if r := ta.call("GET", p, nil); r.code != 200 {
			t.Errorf("%s: %d", p, r.code)
		}
	}
	r := ta.call("POST", "/config/v1/holidays", map[string]any{"date": "2026-02-02"}, bearer(apiToken)...)
	if r.header.Get("Location") != "/config/v1/holidays/3" {
		t.Fatalf("Location under base path: %v", r.header.Get("Location"))
	}
}

func TestRequestCaptureAndViewerMasking(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	ta.call("POST", "/v1/holidays", map[string]any{"date": "2026-12-25"}, append(bearer(apiToken), "X-Request-Id", "req-abcdef123")...)
	ta.sink.Close()
	rows, _, _ := QueryLogs(t.Context(), ta.db, LogQuery{Kinds: []string{"api"}, Limit: 10})
	if len(rows) != 1 || !rows[0].HasPayload || rows[0].Client != "token-1" || rows[0].Attrs["request_id"] != "req-abcdef123" {
		t.Fatalf("log row: %+v", rows)
	}
	id := fmt.Sprint(rows[0].ID)
	ed := ta.console("GET", "/api/logs/"+id, editorToken, nil)
	vi := ta.console("GET", "/api/logs/"+id, viewerToken, nil)
	if !strings.Contains(string(ed.body), apiToken) || !strings.Contains(string(ed.body), `2026-12-25`) {
		t.Fatalf("editor sees the raw request: %s", ed.body)
	}
	if strings.Contains(string(vi.body), apiToken) || !strings.Contains(string(vi.body), Mask) || !strings.Contains(string(vi.body), `"masked":true`) {
		t.Fatalf("viewer sees credentials masked: %s", vi.body)
	}
}
