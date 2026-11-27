package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/skpjr001/fluid-config/internal/config"
)

// ---------- JSON Schemas stay in step with the Go structs ----------

type jsonSchema map[string]any

func loadSchema(t *testing.T, name string) jsonSchema {
	t.Helper()
	b, err := os.ReadFile(repoFile("schema/" + name))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonSchema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

func resolve(root, node jsonSchema) jsonSchema {
	if ref, ok := node["$ref"].(string); ok {
		return root["$defs"].(map[string]any)[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
	}
	return node
}

func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func structOf(t reflect.Type) (reflect.Type, bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	return t, t.Kind() == reflect.Struct && t != reflect.TypeOf(time.Time{}) && t != reflect.TypeOf(config.Duration(0))
}

func compareSchema(t *testing.T, path string, root, node jsonSchema, typ reflect.Type) {
	node = resolve(root, node)
	if node["additionalProperties"] != false {
		t.Errorf("%s: objects must set additionalProperties: false", path)
	}
	props, _ := node["properties"].(map[string]any)
	fields := jsonFields(typ)
	var missing, extra []string
	for name := range fields {
		if _, ok := props[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range props {
		if _, ok := fields[name]; !ok {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing)+len(extra) > 0 {
		t.Errorf("%s: schema lacks %v, has unknown %v", path, missing, extra)
	}
	for name, ft := range fields {
		st, isStruct := structOf(ft)
		p, ok := props[name].(map[string]any)
		if !isStruct || !ok {
			continue
		}
		child := resolve(root, p)
		switch {
		case child["type"] == "array":
			child = resolve(root, child["items"].(map[string]any))
		case ft.Kind() == reflect.Map:
			child = resolve(root, child["additionalProperties"].(map[string]any))
		}
		compareSchema(t, path+"."+name, root, child, st)
	}
}

func TestSchemasMatchStructs(t *testing.T) {
	for file, v := range map[string]any{
		"config.schema.json": config.StaticConfig{}, "api.schema.json": APISpec{},
		"admin.schema.json": AdminConfig{}, "access.schema.json": AccessConfig{},
	} {
		s := loadSchema(t, file)
		if s["$schema"] != "https://json-schema.org/draft/2020-12/schema" || s["$id"] != config.SchemaBase+file {
			t.Errorf("%s: bad $schema/$id", file)
		}
		compareSchema(t, file, s, s, reflect.TypeOf(v))
	}
}

func TestExamplesAreValidAndReferenceSchemas(t *testing.T) {
	t.Setenv("HOLIDAYS_TOKEN", "example-token-value")
	files, _ := filepath.Glob(repoFile("examples/dynamic/*.json"))
	apis, _ := filepath.Glob(repoFile("examples/dynamic/apis/*.json"))
	more, _ := filepath.Glob(repoFile("examples/dynamic/apis/*/*.json"))
	for _, f := range append(append(append(files, apis...), more...), repoFile("examples/config.example.json"), repoFile("examples/config.docker.json")) {
		var m map[string]any
		b, _ := os.ReadFile(f)
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if s, _ := m["$schema"].(string); !strings.HasSuffix(s, ".schema.json") {
			t.Errorf("%s: missing $schema", f)
		}
	}
	snap := loadDir(repoFile("examples/dynamic"), nil)
	if len(snap.errors) > 0 || len(snap.apis) != 2 {
		t.Fatalf("examples: %v %d", snap.errors, len(snap.apis))
	}
	for _, f := range []string{repoFile("examples/config.example.json"), repoFile("examples/config.docker.json")} {
		if _, err := config.Load(f); err != nil {
			t.Error(err)
		}
	}
}

func TestSchemaServedByConsole(t *testing.T) {
	ta := newTestApp(t, nil, nil)
	r := do(t, "GET", ta.admin.URL+"/api/schema/api.schema.json", nil)
	if r.code != 200 || !strings.Contains(string(r.body), `"allowed_ips"`) {
		t.Fatalf("%d", r.code)
	}
	if r := do(t, "GET", ta.admin.URL+"/api/schema/../main.go", nil); r.code == 200 {
		t.Fatal("path traversal")
	}
}

// ---------- config ----------

func TestConfigValidationAndLegacyHints(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "c.json")
		os.WriteFile(p, []byte(s), 0o640)
		return p
	}
	for in, want := range map[string]string{
		`{"addr": ":8090"}`:                      "api_addr",
		`{"apis_dir": "./apis"}`:                 "dynamic_dir",
		`{"trust_forwarded_for": true}`:          "trusted_proxies",
		`{"catalog_token": "x"}`:                 "public_catalog",
		`{"api_addr": ":1", "admin_addr": ":1"}`: "must differ",
		`{"shutdown_timeout": "1h"}`:             "shutdown_timeout",
		`{"database": {"log_level": "loud"}}`:    "log_level",
		`{"api_base_path": "/a/../b"}`:           "api_base_path",
	} {
		_, err := config.Load(write(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", in, err, want)
		}
	}
	cfg, err := config.Load(filepath.Join(dir, "missing.json"))
	if err != nil || cfg.APIAddr != ":8090" || cfg.AdminAddr != "127.0.0.1:9090" {
		t.Fatalf("defaults: %v %+v", err, cfg)
	}
}

// ---------- bootstrap ----------

func TestBootstrapCreatesEverything(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "etc", "config.json")
	cfg := config.Default()
	cfg.DynamicDir = filepath.Join(dir, "dynamic")
	cfg.Database.Path = filepath.Join(dir, "data", "x.db")
	items := bootstrap(cfgPath, cfg, false, false, true)
	for _, p := range []string{cfgPath, filepath.Join(cfg.DynamicDir, "apis", "example.json"), filepath.Join(cfg.DynamicDir, "admin.json"),
		filepath.Join(cfg.DynamicDir, "access.json"), filepath.Join(dir, "data")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s not created: %v", p, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(cfg.DynamicDir, "admin.json")); fi.Mode().Perm() != 0o600 {
		t.Errorf("admin.json mode %v", fi.Mode().Perm())
	}
	var warn int
	for _, it := range items {
		if it.Level == "warn" {
			warn++
		}
	}
	if warn > 0 {
		t.Fatalf("%+v", items)
	}
	snap := loadDir(cfg.DynamicDir, nil)
	if len(snap.errors) > 0 || snap.apis["v1/example"] == nil || len(snap.admin.Tokens) != 1 || snap.admin.Tokens[0].Role != RoleEditor {
		t.Fatalf("bootstrapped files: %v %+v", snap.errors, snap.admin)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatal(err)
	}
	// second run: nothing to do, nothing overwritten
	if again := bootstrap(cfgPath, cfg, true, false, true); len(again) != 0 {
		t.Fatalf("second run: %+v", again)
	}
	// with apis already present, no example is added
	dir2 := t.TempDir()
	os.MkdirAll(filepath.Join(dir2, "apis"), 0o750)
	os.WriteFile(filepath.Join(dir2, "apis", "mine.json"), []byte(`{"name":"mine","version":"v1"}`), 0o640)
	cfg.DynamicDir = dir2
	bootstrap(cfgPath, cfg, true, true, true)
	if _, err := os.Stat(filepath.Join(dir2, "apis", "example.json")); err == nil {
		t.Fatal("example added next to existing APIs")
	}
	var ac AdminConfig
	readStrict(filepath.Join(dir2, "admin.json"), &ac)
	if len(ac.Tokens) != 0 {
		t.Fatal("with FLUID_ADMIN_TOKEN set, no token is generated")
	}
}

func TestContainerDefaults(t *testing.T) {
	cfg := config.Default()
	containerDefaults(cfg, false, hostEnv{Container: "docker"})
	if cfg.AdminAddr != ":9090" {
		t.Fatal(cfg.AdminAddr)
	}
	cfg = config.Default()
	containerDefaults(cfg, true, hostEnv{Container: "docker"})
	containerDefaults(cfg, false, hostEnv{})
	if cfg.AdminAddr != "127.0.0.1:9090" {
		t.Fatal("an existing config, or a host install, keeps loopback")
	}
}

// ---------- graceful shutdown with real listeners ----------

func TestServeAndGracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.APIAddr, cfg.AdminAddr = "127.0.0.1:0", "127.0.0.1:0"
	cfg.DynamicDir = filepath.Join(dir, "dynamic")
	cfg.Database.Path = filepath.Join(dir, "data", "t.db")
	cfg.ShutdownTimeout = config.Duration(3 * time.Second)
	cfg.ReloadInterval = config.Duration(50 * time.Millisecond)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	bootstrap(filepath.Join(dir, "config.json"), cfg, false, false, true)
	ctx, cancel := context.WithCancel(context.Background())
	app, err := NewApp(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx) }()
	select {
	case <-app.listening:
	case err := <-done:
		t.Fatal(err)
	}
	base := "http://" + app.addrs["api"]
	if r := do(t, "GET", base+"/v1/example", nil); r.code != 200 || !strings.Contains(string(r.body), "first record") {
		t.Fatalf("example api: %d %s", r.code, r.body)
	}
	// hot reload: a new file is served without a restart
	os.WriteFile(filepath.Join(cfg.DynamicDir, "apis", "late.json"), []byte(`{"name":"late","version":"v1","data":[{"id":"a"}]}`), 0o640)
	eventually(t, "hot reload", func() bool { return do(t, "GET", base+"/v1/late/a", nil).code == 200 })

	// a slow upload is in flight when the stop signal comes; it still completes
	tok := app.store.Admin().Tokens[0].Token
	pr, pw := io.Pipe()
	got := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", "http://"+app.addrs["admin"]+"/api/apis/v1/example/records", pr)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- 0
			return
		}
		res.Body.Close()
		got <- res.StatusCode
	}()
	pw.Write([]byte(`{"name": "written during`))
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()
	eventually(t, "draining", app.draining.Load)
	pw.Write([]byte(` shutdown"}`))
	pw.Close()
	if code := <-got; code != 201 {
		t.Fatalf("in-flight write during shutdown: %d", code)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("shutdown should end when the work does: %s", took)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.DynamicDir, "apis", "example.json"))
	if !bytes.Contains(b, []byte("written during shutdown")) {
		t.Fatal("the write did not reach the file")
	}
}
