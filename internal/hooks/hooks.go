// Package hooks runs your own code before and after startup and shutdown,
// without letting that code break either.
//
// Every hook is isolated: an error or a panic is logged and never stops the
// agent, and a hook that is still running when its timeout passes is left
// behind (its context is cancelled) while startup or shutdown carries on.
package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"
)

// DefaultTimeout bounds a hook when Hooks.Timeout is not set.
const DefaultTimeout = 10 * time.Second

// Func is one hook. ctx is cancelled when the hook's timeout passes; a hook
// that does slow work should watch it.
type Func func(ctx context.Context) error

// Hooks are the four lifecycle points. A nil Func is skipped.
type Hooks struct {
	// BeforeStartup runs once config is loaded and logging is ready, before
	// any listener opens. Startup waits for it, at most Timeout.
	BeforeStartup Func
	// AfterStartup runs once every listener accepts connections. It runs in
	// the background: requests are served while it runs, and it is cancelled
	// if shutdown begins first.
	AfterStartup Func
	// BeforeShutdown runs when a stop signal arrives, once /readyz fails and
	// before connections drain. It runs alongside shutdown_delay, so it only
	// lengthens shutdown when it takes longer than that delay (at most Timeout).
	BeforeShutdown Func
	// AfterShutdown runs once listeners are closed and in-flight requests
	// have finished, before the logs are flushed and the database is closed.
	// Exit waits for it, at most Timeout.
	AfterShutdown Func

	// Timeout bounds each hook; zero means DefaultTimeout.
	Timeout time.Duration
}

// Run runs fn as the hook name and returns when it finishes or when its
// timeout passes or parent is cancelled, whichever comes first. Errors and
// panics are logged, never returned.
func (h Hooks) Run(parent context.Context, name string, fn Func) {
	if fn == nil {
		return
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1) // buffered: a hook left behind can still finish
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- fmt.Errorf("panic: %v\n%s", p, debug.Stack())
			}
		}()
		done <- fn(ctx)
	}()
	took := func() string { return time.Since(start).Round(time.Millisecond).String() }
	select {
	case err := <-done:
		if err != nil {
			slog.Error("hook failed; carrying on", "kind", "system", "hook", name, "took", took(), "err", err)
			return
		}
		slog.Info("hook done", "kind", "system", "hook", name, "took", took())
	case <-ctx.Done():
		if parent.Err() != nil {
			slog.Warn("hook cancelled: fluid-config is stopping", "kind", "system", "hook", name, "took", took())
			return
		}
		slog.Warn("hook did not finish in time; carrying on without it", "kind", "system", "hook", name, "timeout", timeout.String())
	}
}

// Start runs fn like Run, in the background. The returned channel is closed
// when Run returns.
func (h Hooks) Start(parent context.Context, name string, fn Func) <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); h.Run(parent, name, fn) }()
	return done
}
