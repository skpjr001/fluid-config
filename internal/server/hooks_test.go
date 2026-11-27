package server

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skpjr001/fluid-config/internal/config"
	"github.com/skpjr001/fluid-config/internal/hooks"
)

func TestLifecycleHooksRunInOrderAndCannotBreakStartupOrShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.APIAddr, cfg.AdminAddr = "127.0.0.1:0", "127.0.0.1:0"
	cfg.DynamicDir = filepath.Join(dir, "dynamic")
	cfg.Database.Path = filepath.Join(dir, "data", "t.db")
	cfg.ShutdownTimeout = config.Duration(time.Second)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	bootstrap(filepath.Join(dir, "config.json"), cfg, false, false, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app, err := NewApp(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var order []string
	mark := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	var drainingAtBefore atomic.Bool
	stuck := make(chan struct{})
	defer close(stuck)
	afterStartup := make(chan struct{})
	app.hooks = hooks.Hooks{
		Timeout: 200 * time.Millisecond,
		BeforeStartup: func(context.Context) error {
			mark("before_startup")
			panic("a broken hook must not stop startup")
		},
		AfterStartup: func(context.Context) error {
			mark("after_startup")
			close(afterStartup)
			return errors.New("logged, not fatal")
		},
		BeforeShutdown: func(context.Context) error {
			mark("before_shutdown")
			drainingAtBefore.Store(app.draining.Load())
			<-stuck // ignores ctx: shutdown must go on at the timeout
			return nil
		},
		AfterShutdown: func(context.Context) error {
			mark("after_shutdown")
			return nil
		},
	}

	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx) }()
	select {
	case <-app.listening:
	case err := <-done:
		t.Fatalf("a panicking before_startup hook stopped startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("listeners did not start")
	}
	<-afterStartup
	if r := do(t, "GET", "http://"+app.addrs["api"]+"/v1/example", nil); r.code != 200 {
		t.Fatalf("serving after hooks: %d %s", r.code, r.body)
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stuck before_shutdown hook blocked shutdown")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"before_startup", "after_startup", "before_shutdown", "after_shutdown"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	if !drainingAtBefore.Load() {
		t.Fatal("before_shutdown runs once readiness fails")
	}
}
