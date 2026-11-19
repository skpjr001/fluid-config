package main

import (
	"strings"
	"testing"
	"time"
)

func cachedHolidays(ttl int) map[string]any {
	s := holidays()
	s["cache"] = map[string]any{"enabled": true, "ttl_seconds": ttl, "max_entries": 3}
	return s
}

func TestResponseCache(t *testing.T) {
	ta := newTestApp(t, nil, map[string]any{"apis/h.json": cachedHolidays(60)})
	get := func(q string, h ...string) resp { return ta.call("GET", "/v1/holidays"+q, nil, h...) }
	if r := get("?exchange=NSE"); r.header.Get("X-Cache") != "MISS" {
		t.Fatalf("first: %v", r.header)
	}
	r := get("?exchange=NSE")
	if r.header.Get("X-Cache") != "HIT" || r.header.Get("ETag") == "" || !strings.Contains(string(r.body), "2026-01-26") || r.header.Get("X-Request-Id") == "" {
		t.Fatalf("second: %v %s", r.header, r.body)
	}
	if r := get("?exchange=NSE", "If-None-Match", r.header.Get("ETag")); r.code != 304 || r.header.Get("X-Cache") != "HIT" {
		t.Fatalf("304 from cache: %d %v", r.code, r.header)
	}
	if r := get("?exchange=NSE", "Cache-Control", "no-cache"); r.header.Get("X-Cache") != "MISS" {
		t.Fatal("no-cache must bypass the cache")
	}
	// a write changes the data version: the next read is fresh
	ta.call("PATCH", "/v1/holidays/1", map[string]any{"exchange": "BSE"}, bearer(apiToken)...)
	if r := get("?exchange=NSE"); r.header.Get("X-Cache") != "MISS" || strings.Contains(string(r.body), "2026-01-26") {
		t.Fatalf("stale after write: %v %s", r.header, r.body)
	}
	// errors are never cached
	get("/99")
	if r := get("/99"); r.header.Get("X-Cache") == "HIT" {
		t.Fatal("404 cached")
	}
	// LRU bound
	for _, q := range []string{"?a=1", "?a=2", "?a=3", "?a=4"} {
		get(q)
	}
	st := ta.cache.Stats(ta.store.Get("v1/holidays"))
	if st.Entries != 3 || st.Hits < 2 || st.Bytes == 0 {
		t.Fatalf("stats: %+v", st)
	}
	// purge from the console
	p := ta.console("POST", "/api/apis/v1/holidays/cache/purge", editorToken, nil).json(t)
	if p["purged"].(float64) != 3 || p["cache"].(map[string]any)["entries"].(float64) != 0 || p["cache"].(map[string]any)["purged_at"] == nil {
		t.Fatalf("purge: %v", p)
	}
	if r := get("?a=1"); r.header.Get("X-Cache") != "MISS" {
		t.Fatal("purged entry served")
	}
}

func TestCacheNeverWidensAccess(t *testing.T) {
	s := cachedHolidays(60)
	s["auth"] = map[string]any{"type": "bearer", "tokens": []string{apiToken}}
	ta := newTestApp(t, nil, map[string]any{"apis/h.json": s})
	if r := ta.call("GET", "/v1/holidays", nil, bearer(apiToken)...); r.code != 200 {
		t.Fatal(r.code)
	}
	if r := ta.call("GET", "/v1/holidays", nil); r.code != 401 {
		t.Fatalf("cached answer leaked to an unauthenticated caller: %d", r.code)
	}
}

func TestCacheTTLAndDisabled(t *testing.T) {
	c := NewResponseCache()
	spec := &CacheSpec{Enabled: true, TTLSeconds: 1, MaxEntries: 10}
	c.Put("a", spec, &cachedResponse{key: "k", status: 200, body: []byte("x")})
	if c.Get("a", "k") == nil {
		t.Fatal("fresh entry missing")
	}
	c.apis["a"].entries["k"].Value.(*cachedResponse).expires = time.Now().Add(-time.Second)
	if c.Get("a", "k") != nil {
		t.Fatal("expired entry served")
	}
	ta := newTestApp(t, nil, nil)
	if r := ta.call("GET", "/v1/holidays", nil); r.header.Get("X-Cache") != "" {
		t.Fatal("an API without cache must not say X-Cache")
	}
}
