package server

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/skpjr001/fluid-config/internal/config"
)

// First-run bootstrap: create every directory and config file the agent
// needs, so a fresh install (host, systemd, empty container) starts with no
// manual steps. Existing files are never overwritten. Anything that cannot be
// created is reported as a warning and the agent runs on built-in defaults.

// BootstrapItem is one thing bootstrap created or could not create.
type BootstrapItem struct {
	Level   string // "info" (created) or "warn" (could not create)
	Path    string
	Message string
}

// bootstrap creates what is missing. cfgExisted tells whether cfgPath was
// present before loadStatic fell back to defaults.
func bootstrap(cfgPath string, cfg *config.StaticConfig, cfgExisted, envAdminToken bool, examples bool) []BootstrapItem {
	var out []BootstrapItem
	created := func(path, what, suffix string) {
		path = absPath(path)
		out = append(out, BootstrapItem{"info", path, "created " + what + " " + path + suffix})
	}
	failed := func(path, what string, err error) {
		path = absPath(path)
		out = append(out, BootstrapItem{"warn", path, fmt.Sprintf("could not create %s %s: %v", what, path, err)})
	}
	mkdir := func(dir string, perm fs.FileMode, what string) bool {
		dir = filepath.Clean(dir)
		if dir == "." {
			return true
		}
		if fi, err := os.Stat(dir); err == nil {
			if !fi.IsDir() {
				failed(dir, what, errors.New("a file with that name exists"))
				return false
			}
			return true
		}
		if err := os.MkdirAll(dir, perm); err != nil {
			failed(dir, what, err)
			return false
		}
		created(dir, what, "")
		return true
	}
	writeNew := func(path, what string, v any, perm fs.FileMode) {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return // exists (or unreadable: leave it for the loader to report)
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err == nil {
			err = writeAtomicMode(path, b, perm)
		}
		if err != nil {
			failed(path, what, err)
			return
		}
		created(path, what, " with safe defaults")
	}

	// config.json
	if !cfgExisted {
		if mkdir(filepath.Dir(cfgPath), 0o750, "config directory") {
			writeNew(cfgPath, "static config", defaultConfigFile(cfg), 0o640)
		}
	}

	// database directory
	mkdir(filepath.Dir(cfg.Database.Path), 0o750, "database directory")

	// ACME certificate caches
	acmeDefault := filepath.Join(filepath.Dir(cfg.Database.Path), "acme")
	for _, t := range []*config.TLSConfig{cfg.APITLS, cfg.AdminTLS} {
		if t != nil && t.ACME != nil {
			dir := t.ACME.CacheDir
			if dir == "" {
				dir = acmeDefault
			}
			mkdir(dir, 0o700, "ACME certificate cache")
		}
	}

	// dynamic dir, apis/ (with an example API) and the two singleton files
	if !mkdir(cfg.DynamicDir, 0o750, "dynamic config directory") {
		return out
	}
	if mkdir(filepath.Join(cfg.DynamicDir, apisSubdir), 0o750, "API files directory") && examples {
		if files, err := listAPIFiles(filepath.Join(cfg.DynamicDir, apisSubdir)); err == nil && len(files) == 0 {
			writeNew(filepath.Join(cfg.DynamicDir, apisSubdir, "example.json"), "example API", exampleAPI(), 0o640)
		}
	}
	writeNew(filepath.Join(cfg.DynamicDir, "access.json"), "access allowlists",
		defaultAccessFile(), 0o640)
	adminPath := filepath.Join(cfg.DynamicDir, "admin.json")
	admin := &AdminConfig{Schema: config.SchemaBase + "admin.schema.json", Tokens: []AdminToken{}}
	if !envAdminToken {
		admin.Tokens = append(admin.Tokens, AdminToken{Name: "admin", Token: randomToken(), Role: RoleEditor})
	}
	before := len(out)
	writeNew(adminPath, "admin config", admin, 0o600)
	if len(out) > before && out[len(out)-1].Level == "info" && !envAdminToken {
		out[len(out)-1].Message += `; it holds a generated editor token named "admin": read it from that file to sign in to the console (fluid-config token prints it)`
	}
	return out
}

// containerDefaults adjusts the built-in defaults when fluid-config writes
// its own config.json inside a container: a loopback-only console could
// not be reached through a published port. The console allowlist
// (access.json, restricted default) still limits who gets in.
func containerDefaults(cfg *config.StaticConfig, cfgExisted bool, env hostEnv) {
	if !cfgExisted && env.Container != "" && cfg.AdminAddr == config.Default().AdminAddr {
		cfg.AdminAddr = ":9090"
	}
}

// defaultConfigFile is the config.json written on first run: every setting
// spelled out with its default, so it is easy to edit.
func defaultConfigFile(cfg *config.StaticConfig) *config.StaticConfig {
	c := *cfg
	c.Schema = config.SchemaBase + "config.schema.json"
	if c.TrustedProxies == nil {
		c.TrustedProxies = []string{}
	}
	return &c
}

// absPath makes log messages unambiguous ("./dynamic" says little in a journal).
func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func logBootstrap(items []BootstrapItem) {
	for _, it := range items {
		level := slog.LevelInfo
		if it.Level == "warn" {
			level = slog.LevelWarn
		}
		slog.Log(context.Background(), level, it.Message, "kind", "deploy", "topic", "bootstrap", "path", it.Path)
	}
}

// pathPresent reports whether path exists (a directory counts, so loadStatic
// can report it instead of bootstrap writing next to it).
func pathPresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// cmdInit creates missing directories and config files, then validates them.
func cmdInit(args []string, out *os.File) error {
	fset := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := fset.String("config", "config.json", "static config file")
	noExample := fset.Bool("no-example", false, "do not create apis/example.json in an empty apis/ directory")
	fset.Parse(args)
	existed := pathPresent(*cfgPath)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	containerDefaults(cfg, existed, detectEnv())
	items := bootstrap(*cfgPath, cfg, existed, envAdminToken() != "", !*noExample)
	warns := 0
	for _, it := range items {
		fmt.Fprintf(out, "%-5s %s\n", map[string]string{"info": "OK", "warn": "WARN"}[it.Level], it.Message)
		if it.Level == "warn" {
			warns++
		}
	}
	if len(items) == 0 {
		fmt.Fprintln(out, "nothing to create: every directory and config file exists")
	}
	if problems := checkConfig(*cfgPath); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(out, p)
		}
		return fmt.Errorf("%d invalid file(s)", len(problems))
	}
	if warns > 0 {
		return fmt.Errorf("%d item(s) could not be created", warns)
	}
	return nil
}

// exampleAPI is written to an empty apis/ on first start so there is
// something to open in the console; it is an ordinary file, delete it freely.
func exampleAPI() *APISpec {
	t := true
	one := 1.0
	return &APISpec{
		Schema: config.SchemaBase + "api.schema.json", Name: "example", Version: "v1",
		Description: "An example API created on first start: edit or delete apis/example.json",
		Enabled:     &t, Methods: []string{"GET"}, // read-only; add methods and auth to allow writes
		Auth:  AuthSpec{Type: "none"},
		Cache: &CacheSpec{Enabled: true, TTLSeconds: 60, MaxEntries: 500},
		Fields: map[string]*FieldSpec{
			"name":   {Type: "string", Required: true, Min: &one},
			"active": {Type: "boolean"},
		},
		Data: []map[string]any{{"id": json.Number("1"), "name": "first record", "active": true}},
	}
}
