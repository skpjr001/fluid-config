package hooks

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureLogs sends slog output to a buffer for the test's duration.
func captureLogs(t *testing.T) func() string {
	var mu sync.Mutex
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func TestHooksNeverFailTheCaller(t *testing.T) {
	logs := captureLogs(t)
	h := Hooks{Timeout: 50 * time.Millisecond}
	ran := false
	h.Run(context.Background(), "ok", func(context.Context) error { ran = true; return nil })
	h.Run(context.Background(), "err", func(context.Context) error { return errors.New("boom") })
	h.Run(context.Background(), "panic", func(context.Context) error { panic("kaboom") })
	h.Run(context.Background(), "nil", nil)

	start := time.Now()
	block := make(chan struct{})
	defer close(block)
	h.Run(context.Background(), "stuck", func(context.Context) error { <-block; return nil }) // ignores ctx
	if d := time.Since(start); d > time.Second {
		t.Fatalf("a stuck hook held the caller for %v", d)
	}

	out := logs()
	for _, want := range []string{"hook=ok", `hook=err`, "boom", "hook=panic", "kaboom", "hook=stuck", "did not finish in time"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	if !ran || strings.Contains(out, "hook=nil") {
		t.Fatalf("ran=%v logs:\n%s", ran, out)
	}
}

func TestHookIsCancelledWithItsParent(t *testing.T) {
	logs := captureLogs(t)
	parent, cancel := context.WithCancel(context.Background())
	sawCancel := make(chan struct{})
	done := Hooks{}.Start(parent, "bg", func(ctx context.Context) error {
		<-ctx.Done()
		close(sawCancel)
		return ctx.Err()
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after the parent was cancelled")
	}
	<-sawCancel
	if !strings.Contains(logs(), "fluid-config is stopping") {
		t.Fatal(logs())
	}
}
