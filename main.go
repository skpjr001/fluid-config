package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfgPath := flag.String("config", "config.json", "static config file")
	check := flag.Bool("check", false, "validate all api files and exit (use in CI / pre-commit)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cfg, err := loadStatic(*cfgPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	if *check {
		os.Exit(runCheck(cfg.APIsDir))
	}
	store, err := NewStore(cfg.APIsDir)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	stop := make(chan struct{})
	go store.Watch(cfg.reloadEvery, stop)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           &Server{store: store, cfg: cfg, limiter: newLimiter()},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		slog.Info("listening", "addr", cfg.Addr, "apis_dir", cfg.APIsDir, "base_path", cfg.BasePath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// runCheck parses every file strictly and reports all problems.
func runCheck(dir string) int {
	s := &Store{dir: dir}
	files, err := s.listFiles()
	if err != nil {
		slog.Error("check", "err", err)
		return 1
	}
	seen := map[string]string{}
	bad := 0
	for _, f := range files {
		spec, err := parseSpecFile(f)
		if err != nil {
			slog.Error("INVALID", "file", f, "err", err)
			bad++
			continue
		}
		if other, dup := seen[spec.key()]; dup {
			slog.Error("DUPLICATE", "key", spec.key(), "file", f, "also_in", other)
			bad++
			continue
		}
		seen[spec.key()] = f
		slog.Info("ok", "file", f, "api", spec.key(), "records", len(spec.Data))
	}
	if bad > 0 {
		return 1
	}
	return 0
}
