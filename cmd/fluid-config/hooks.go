package main

import (
	"context"
	"time"

	"github.com/skpjr001/fluid-config/internal/hooks"
)

// lifecycle is where your own startup and shutdown code goes. Fill in any of
// the four blocks; an empty block does nothing.
//
// Hooks cannot break fluid-config: an error or a panic is logged and it
// carries on, and a hook still running after Timeout is left behind (its ctx
// is cancelled) while startup or shutdown continues. Watch ctx in slow work.
// Log with log/slog: lines reach stdout and the console's Logs page.
var lifecycle = hooks.Hooks{
	Timeout: 10 * time.Second, // per hook

	// Config is loaded and logging is ready; no listener is open yet.
	// Startup waits for this block (at most Timeout).
	BeforeStartup: func(ctx context.Context) error {
		return nil
	},

	// Every listener is accepting requests. Runs in the background while
	// requests are served; cancelled if shutdown begins first.
	AfterStartup: func(ctx context.Context) error {
		return nil
	},

	// A stop signal arrived and /readyz now fails; connections have not
	// started draining. Runs alongside shutdown_delay.
	BeforeShutdown: func(ctx context.Context) error {
		return nil
	},

	// Listeners are closed and in-flight requests finished; logs are not yet flushed
	// and the database is still open. Exit waits for this block (at most Timeout).
	AfterShutdown: func(ctx context.Context) error {
		return nil
	},
}
