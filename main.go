package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

//go:embed all:ui
var uiFS embed.FS

//go:embed schema/*.json
var schemaFS embed.FS

// App holds the shared pieces both listeners use.
type App struct {
	cfg     *StaticConfig
	store   *Store
	db      *DB
	sink    *LogSink
	hub     *Hub
	metrics *Metrics
	audit   *AuditLog
	cache   *ResponseCache
	api     *API
	admin   *Admin
	started time.Time
	env     hostEnv
	notes   []DeployNote

	draining  atomic.Bool       // shutting down: /readyz answers 503
	addrs     map[string]string // listener name -> bound address; set before listening is closed
	listening chan struct{}     // closed once every listener accepts connections
}

const usage = `fluid-config — drop a JSON file in apis/ and get a REST API, with a console

Usage:
  fluid-config [serve] [-config config.json]   run the server (default)
  fluid-config init    [-config config.json]   create missing directories and config files (serve does this too)
  fluid-config check   [-config config.json]   validate config.json, admin.json, access.json and every API file
  fluid-config token   [-config config.json]   print the console tokens from admin.json
  fluid-config logs    [flags]                 query the SQLite request log (fluid-config logs -h)
  fluid-config healthcheck [-config ...]       exit 0 if the local server is live (for Docker)
  fluid-config version                         print the version
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "init":
		err = cmdInit(args, os.Stdout)
	case "check":
		err = cmdCheck(args, os.Stdout)
	case "token":
		err = cmdToken(args, os.Stdout)
	case "logs":
		err = cmdLogs(args, os.Stdout)
	case "healthcheck":
		err = cmdHealthcheck(args)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "static config file")
	noExample := fs.Bool("no-example", false, "do not create apis/example.json in an empty apis/ directory")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Parse(args)
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	existed := pathPresent(*cfgPath)
	cfg, err := loadStatic(*cfgPath)
	if err != nil {
		return err
	}
	containerDefaults(cfg, existed, detectEnv())
	created := bootstrap(*cfgPath, cfg, existed, envAdminToken() != "", !*noExample)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore default handling: a second signal ends the process at once
	}()
	app, err := NewApp(ctx, cfg)
	if err != nil {
		return err
	}
	logBootstrap(created)
	return app.Serve(ctx)
}

// NewApp opens the database, loads the dynamic config and builds every component.
func NewApp(ctx context.Context, cfg *StaticConfig) (*App, error) {
	app := &App{cfg: cfg, hub: NewHub(), metrics: NewMetrics(), cache: NewResponseCache(), started: time.Now(), listening: make(chan struct{})}
	app.metrics.registerHelp()
	stdout := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(stdout))

	db, err := OpenDB(cfg.Database.Path)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	app.db = db
	app.audit = &AuditLog{db: db, hub: app.hub}
	app.sink = NewLogSink(db, levelByName[strings.ToLower(cfg.Database.LogLevel)], app.hub)
	slog.SetDefault(slog.New(NewTeeHandler(stdout, app.sink)))

	store, err := NewStore(cfg.DynamicDir)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("dynamic config: %w", err)
	}
	app.store = store
	app.api = &API{app: app, limiter: NewRateLimiter()}
	app.admin = &Admin{app: app}
	var seen map[string]bool
	store.OnReload(func(ok bool, changes []AuditEntry) {
		// Hand edits are part of the history, and their APIs' cached responses go.
		app.audit.Record(context.Background(), changes...)
		purged := map[string]bool{}
		for _, c := range changes {
			if !purged[c.API] {
				purged[c.API] = true
				if store.Get(c.API) == nil {
					app.cache.Forget(c.API)
				} else {
					app.cache.Invalidate(c.API)
				}
			}
		}
		if len(changes) > 0 {
			slog.Info("api files changed on disk", "kind", "config", "changes", len(changes), "apis", strings.Join(sortedSet(purged), ","))
		}
		// Log access notes that are new (e.g. "api x accepts writes from anyone").
		if seen == nil {
			seen = map[string]bool{}
			for _, n := range app.notes {
				seen[n.Level+n.Message] = true
			}
		}
		next, fresh := map[string]bool{}, []DeployNote(nil)
		for _, n := range accessNotes(app.cfg, store.snap.Load(), app.env) {
			next[n.Level+n.Message] = true
			if !seen[n.Level+n.Message] {
				fresh = append(fresh, n)
			}
		}
		seen = next
		if len(fresh) > 0 {
			slog.Info("access rules changed", "kind", "deploy", "topic", "access")
			logDeploymentNotes(fresh)
		}
		res := "ok"
		if !ok {
			res = "error"
		}
		app.metrics.Inc("fluid_config_reloads_total", "result", res)
	})

	app.metrics.Gauge("fluid_log_rows_dropped", "Log rows dropped because the write queue was full.", func() float64 { return float64(app.sink.Dropped()) })
	app.metrics.Gauge("fluid_config_errors", "Config files that currently fail validation.", func() float64 { return float64(len(app.store.Errors())) })
	app.metrics.Gauge("fluid_apis", "APIs loaded.", func() float64 { return float64(len(app.store.All())) })
	app.metrics.Gauge("fluid_cache_bytes", "Memory held by cached API responses.", func() float64 { return float64(app.cache.TotalBytes()) })
	return app, nil
}

func (app *App) readyz(w http.ResponseWriter, r *http.Request) {
	if app.draining.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "draining": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := app.db.PingContext(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "error": "database: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "apis": len(app.store.All())})
}

// Serve runs both listeners and the background loops until ctx is cancelled.
func (app *App) Serve(ctx context.Context) error {
	cfg := app.cfg
	app.env = detectEnv()
	app.notes = deploymentNotes(cfg, app.store.snap.Load(), app.env, envAdminToken() != "", time.Now())
	logDeploymentNotes(app.notes)

	bg, cancelBG := context.WithCancel(context.Background())
	var bgWG sync.WaitGroup
	stopWatch := make(chan struct{})
	bgWG.Add(2)
	go func() { defer bgWG.Done(); app.store.Watch(cfg.ReloadInterval.D(), stopWatch) }()
	go func() { defer bgWG.Done(); app.housekeeping(bg) }()

	running, err := app.openListeners()
	if err != nil {
		cancelBG()
		close(stopWatch)
		bgWG.Wait()
		return err
	}
	errc := make(chan error, len(running))
	app.addrs = map[string]string{}
	for _, rs := range running {
		app.addrs[rs.name] = rs.ln.Addr().String()
		slog.Info("listening", "kind", "system", "server", rs.name, "addr", rs.ln.Addr().String(), "scheme", rs.scheme)
		go func(rs runningServer) {
			if err := rs.srv.Serve(rs.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", rs.name, err)
			}
		}(rs)
	}
	slog.Info("fluid-config started", "kind", "system", "version", version, "apis", len(app.store.All()))
	close(app.listening)

	select {
	case <-ctx.Done():
	case err = <-errc:
		slog.Error("server failed", "kind", "system", "err", err)
	}
	app.shutdown(running, stopWatch, cancelBG, &bgWG)
	return err
}

// housekeeping prunes old rows and idle rate-limit buckets.
func (app *App) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	sweep := time.NewTicker(5 * time.Minute)
	defer sweep.Stop()
	prune := func() {
		day := 24 * time.Hour
		n, err := Prune(ctx, app.db, time.Duration(app.cfg.Database.RetentionDays)*day, time.Duration(app.cfg.Database.AuditRetentionDays)*day)
		if err != nil && ctx.Err() == nil {
			slog.Error("prune failed", "kind", "system", "err", err)
		} else if n > 0 {
			slog.Info("pruned old rows", "kind", "system", "rows", n)
		}
	}
	prune()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		case <-sweep.C:
			app.api.limiter.Sweep(10 * time.Minute)
		}
	}
}

// shutdown stops gracefully:
//  1. /readyz starts failing; with shutdown_delay the listeners keep serving
//     that long so load balancers stop sending new requests;
//  2. live streams end, listeners stop accepting, and in-flight requests
//     (including writes to API files) get shutdown_timeout to finish;
//  3. at the deadline, remaining connections are closed;
//  4. logs are flushed and the database is closed.
//
// A write that started always completes or fails as a whole: files are
// replaced atomically, so a stop never leaves a half-written API file.
// A second SIGINT/SIGTERM ends the process at once.
func (app *App) shutdown(running []runningServer, stopWatch chan struct{}, cancelBG context.CancelFunc, bgWG *sync.WaitGroup) {
	start := time.Now()
	timeout, delay := app.cfg.ShutdownTimeout.D(), app.cfg.ShutdownDelay.D()
	app.draining.Store(true)
	slog.Info("shutting down gracefully (send the signal again to stop at once)", "kind", "system",
		"timeout", timeout.String(), "delay", delay.String())
	if delay > 0 {
		slog.Info("readiness now fails; still serving during shutdown_delay", "kind", "system", "delay", delay.String())
		time.Sleep(delay)
	}
	deadline, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	app.hub.Close() // live streams never go idle; end them so Shutdown can finish
	close(stopWatch)
	cancelBG()

	var wg sync.WaitGroup
	var forced atomic.Int64
	for _, rs := range running {
		wg.Add(1)
		go func(rs runningServer) {
			defer wg.Done()
			if err := rs.srv.Shutdown(deadline); err != nil {
				forced.Add(1)
				rs.srv.Close() // deadline reached: close what is left
			}
		}(rs)
	}
	wg.Wait()
	bgWG.Wait()
	if n := forced.Load(); n > 0 {
		slog.Warn("shutdown timeout reached: closed remaining connections", "kind", "system", "listeners", n)
	}
	slog.Info("shutdown complete", "kind", "system", "took", time.Since(start).Round(time.Millisecond).String())
	app.sink.Close()
	app.db.Close()
}

// ---------- check ----------

func cmdCheck(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "static config file")
	fs.Parse(args)
	problems := checkConfig(*cfgPath)
	for _, p := range problems {
		fmt.Fprintln(out, p)
	}
	if cfg, err := loadStatic(*cfgPath); err == nil {
		if !pathPresent(*cfgPath) {
			fmt.Fprintf(out, "%s does not exist yet: built-in defaults apply, and it is created on first start (or run fluid-config init)\n", *cfgPath)
		}
		if !pathPresent(cfg.DynamicDir) {
			fmt.Fprintf(out, "%s does not exist yet: it is created on first start (or run fluid-config init)\n", cfg.DynamicDir)
		}
		snap := loadDir(cfg.DynamicDir, nil)
		for _, s := range snap.all() {
			fmt.Fprintf(out, "ok    %-28s %s/%s (%d records)\n", s.key(), apisSubdir, s.rel, len(s.Data))
		}
		ns := deploymentNotes(cfg, snap, detectEnv(), envAdminToken() != "", time.Now())
		fmt.Fprintln(out, "\nDeployment notes:")
		for _, n := range ns {
			fmt.Fprintf(out, "  %-5s [%s] %s\n", strings.ToUpper(n.Level), n.Topic, n.Message)
		}
	}
	if len(problems) == 0 {
		fmt.Fprintln(out, "\nOK: all config files are valid")
		return nil
	}
	return fmt.Errorf("%d invalid file(s)", len(problems))
}

// checkConfig validates the static file and every dynamic file without
// touching the database; it returns "path: problem" lines.
func checkConfig(cfgPath string) []string {
	cfg, err := loadStatic(cfgPath)
	if err != nil {
		return []string{err.Error()}
	}
	if fi, err := os.Stat(cfg.DynamicDir); err == nil && !fi.IsDir() {
		return []string{cfg.DynamicDir + ": not a directory"}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return []string{fmt.Sprintf("%s: %v", cfg.DynamicDir, err)}
	}
	snap := loadDir(cfg.DynamicDir, nil)
	var out []string
	for f, e := range snap.errors {
		out = append(out, filepath.ToSlash(filepath.Join(cfg.DynamicDir, f))+": "+e)
	}
	sort.Strings(out)
	return out
}

// ---------- token ----------

// cmdToken prints the console tokens, so a container install can read them
// with `docker exec fluid-config /fluid-config token`.
func cmdToken(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "static config file")
	fs.Parse(args)
	cfg, err := loadStatic(*cfgPath)
	if err != nil {
		return err
	}
	var ac AdminConfig
	if err := readStrict(filepath.Join(cfg.DynamicDir, "admin.json"), &ac); err != nil {
		return fmt.Errorf("admin.json: %w", err)
	}
	if len(ac.Tokens) == 0 {
		return errors.New("admin.json has no tokens")
	}
	for _, t := range ac.Tokens {
		fmt.Fprintf(out, "%-8s %-16s %s\n", t.Role, t.Name, t.Token)
	}
	return nil
}

// ---------- healthcheck ----------

func cmdHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	cfgPath := fs.String("config", "config.json", "static config file (to find the API port)")
	url := fs.String("url", "", "URL to probe (default http://127.0.0.1:<api port>/livez)")
	fs.Parse(args)
	target := *url
	if target == "" {
		addr := defaultStatic().APIAddr
		scheme := "http"
		if cfg, err := loadStatic(*cfgPath); err == nil {
			addr = cfg.APIAddr
			if cfg.APITLS != nil {
				scheme = "https"
			}
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		target = scheme + "://127.0.0.1:" + port + "/livez"
	}
	c := &http.Client{Timeout: 3 * time.Second, Transport: insecureLocal()}
	resp, err := c.Get(target)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", target, resp.Status)
	}
	return nil
}

// runningServer is a bound listener with the server that will serve it.
type runningServer struct {
	name   string
	scheme string
	srv    *http.Server
	ln     net.Listener
}

// Handlers with base paths applied, as served (also used by tests).
func (app *App) apiHandler() http.Handler {
	// every call is captured in full (RQH/RQB/RSH/RSB); each API checks its allowed_ips
	return app.captureInbound(stripBasePath(app.cfg.APIBasePath, app.api))
}

func (app *App) adminHandler() http.Handler {
	return app.accessGuard(stripBasePath(app.cfg.AdminBasePath, app.admin.Handler()))
}

// openListeners binds both listeners (with TLS where configured) plus any
// ACME HTTP-01 challenge listeners. Nothing is served until Serve runs them.
func (app *App) openListeners() ([]runningServer, error) {
	cfg := app.cfg
	acmeCache := filepath.Join(filepath.Dir(cfg.Database.Path), "acme")
	specs := []struct {
		name string
		srv  *http.Server
		tls  *TLSConfig
	}{
		{"api", &http.Server{Addr: cfg.APIAddr, Handler: app.apiHandler(), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}, cfg.APITLS},
		{"admin", &http.Server{Addr: cfg.AdminAddr, Handler: app.adminHandler(), ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}, cfg.AdminTLS},
	}
	var out []runningServer
	fail := func(err error) ([]runningServer, error) {
		for _, rs := range out {
			rs.ln.Close()
		}
		return nil, err
	}
	for _, s := range specs {
		ts, err := buildTLS(s.tls, acmeCache, false)
		if err != nil {
			return fail(fmt.Errorf("%s TLS: %w", s.name, err))
		}
		scheme := "http"
		if ts != nil {
			scheme = "https"
			s.srv.TLSConfig = ts.config
		}
		ln, err := listen(s.srv.Addr, ts)
		if err != nil {
			if errors.Is(err, syscall.EACCES) {
				err = fmt.Errorf("%w (ports below 1024 need CAP_NET_BIND_SERVICE or root; see deploy/systemd)", err)
			}
			return fail(fmt.Errorf("%s listener: %w", s.name, err))
		}
		out = append(out, runningServer{s.name, scheme, s.srv, ln})
		if ts != nil && ts.httpHandler != nil {
			cl, err := net.Listen("tcp", ts.httpAddr)
			if err != nil {
				return fail(fmt.Errorf("%s ACME challenge listener: %w", s.name, err))
			}
			ch := &http.Server{Handler: ts.httpHandler, ReadHeaderTimeout: 10 * time.Second}
			out = append(out, runningServer{s.name + "-acme-http", "http", ch, cl})
		}
	}
	return out, nil
}
