package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
)

// Capture holds the full exchange of one logged API call: request headers
// (RQH), request body (RQB), response headers (RSH) and response body (RSB).
// Values are stored exactly as sent and received; viewers get credentials
// masked when they read them back (see maskPayload), editors see them raw.
//
// A log record that carries a Capture is written once the capture finishes,
// so a body that is still streaming when the call is logged is complete in
// the stored row. A nil *Capture (capture disabled) is safe to use.
type Capture struct {
	mu          sync.Mutex
	reqHeaders  http.Header
	reqBody     capBuf
	respHeaders http.Header
	respBody    capBuf
	done        bool
	waiters     []func()
}

// capBuf keeps the first max bytes of a body and counts all of them.
type capBuf struct {
	b   []byte
	n   int64
	max int64
}

func (c *capBuf) add(p []byte) {
	c.n += int64(len(p))
	if room := c.max - int64(len(c.b)); room > 0 {
		c.b = append(c.b, p[:min(int64(len(p)), room)]...)
	}
}

func newCapture(maxBody int64) *Capture {
	return &Capture{reqBody: capBuf{max: maxBody}, respBody: capBuf{max: maxBody}}
}

// newCapture returns nil when payload capture is off.
func (app *App) newCapture() *Capture {
	if !app.cfg.Database.CapturePayloads {
		return nil
	}
	return newCapture(app.cfg.Database.CaptureMaxBodyBytes)
}

// Request headers carry HTTP/2-style pseudo headers for the request line.
func requestHeaders(method, target, host string, h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	out[":method"] = []string{method}
	out[":path"] = []string{target}
	if host != "" && out.Get("Host") == "" {
		out["Host"] = []string{host}
	}
	return out
}

func responseHeaders(status int, h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		out = http.Header{}
	}
	out[":status"] = []string{strconv.Itoa(status)}
	return out
}

func (c *Capture) SetRequest(h http.Header, body []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqHeaders = h
	if body != nil {
		c.reqBody.b, c.reqBody.n = nil, 0
		c.reqBody.add(body)
	}
}

func (c *Capture) SetResponse(h http.Header, body []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.respHeaders = h
	if body != nil {
		c.respBody.b, c.respBody.n = nil, 0
		c.respBody.add(body)
	}
}

func (c *Capture) hasRequest() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqHeaders != nil
}

// teeWriter appends streamed body bytes to one side of the capture.
type teeWriter struct {
	c    *Capture
	resp bool
}

func (t teeWriter) Write(p []byte) (int, error) {
	t.c.mu.Lock()
	if t.resp {
		t.c.respBody.add(p)
	} else {
		t.c.reqBody.add(p)
	}
	t.c.mu.Unlock()
	return len(p), nil
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

// TeeRequestBody / TeeResponseBody copy a body into the capture as it is read.
func (c *Capture) TeeRequestBody(rc io.ReadCloser) io.ReadCloser {
	if c == nil || rc == nil || rc == http.NoBody {
		return rc
	}
	return teeReadCloser{io.TeeReader(rc, teeWriter{c, false}), rc}
}

func (c *Capture) TeeResponseBody(rc io.ReadCloser) io.ReadCloser {
	if c == nil || rc == nil {
		return rc
	}
	return teeReadCloser{io.TeeReader(rc, teeWriter{c, true}), rc}
}

// Finish marks the exchange complete and releases the log rows waiting on it.
func (c *Capture) Finish() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.done {
		c.mu.Unlock()
		return
	}
	c.done = true
	ws := c.waiters
	c.waiters = nil
	c.mu.Unlock()
	for _, f := range ws {
		f()
	}
}

// whenDone runs f once the capture finishes (now, if it already has).
func (c *Capture) whenDone(f func()) {
	c.mu.Lock()
	if !c.done {
		c.waiters = append(c.waiters, f)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	f()
}

// LogValue keeps bodies out of stdout: only their sizes are printed.
func (c *Capture) LogValue() slog.Value {
	if c == nil {
		return slog.Value{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return slog.GroupValue(slog.Int64("rq_body_bytes", c.reqBody.n), slog.Int64("rs_body_bytes", c.respBody.n))
}

// snapshot is what the log store writes.
type captureRow struct {
	reqHeaders, respHeaders http.Header
	reqBody, respBody       []byte
	reqSize, respSize       int64
}

func (c *Capture) snapshot() captureRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	return captureRow{c.reqHeaders, c.respHeaders, c.reqBody.b, c.respBody.b, c.reqBody.n, c.respBody.n}
}

// ---------- context + inbound middleware ----------

type captureKey struct{}

func withCapture(ctx context.Context, c *Capture) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, captureKey{}, c)
}

func captureFrom(ctx context.Context) *Capture {
	c, _ := ctx.Value(captureKey{}).(*Capture)
	return c
}

// captureWriter records the response headers and body sent to the client.
type captureWriter struct {
	http.ResponseWriter
	c      *Capture
	status int
}

func (w *captureWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.c.SetResponse(responseHeaders(code, w.Header()), nil)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	teeWriter{w.c, true}.Write(p)
	return w.ResponseWriter.Write(p)
}

func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// captureInbound records every call to the API listener as the
// client sent it and as fluid-config answered it.
func (app *App) captureInbound(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := app.newCapture()
		if c == nil || isHealthPath(r) {
			next.ServeHTTP(w, r)
			return
		}
		defer c.Finish()
		c.SetRequest(requestHeaders(r.Method, r.URL.RequestURI(), r.Host, r.Header), nil)
		body := c.TeeRequestBody(r.Body)
		r.Body = body
		next.ServeHTTP(&captureWriter{ResponseWriter: w, c: c}, r.WithContext(withCapture(r.Context(), c)))
		app.drainInto(body) // requests refused before their body was read are still captured in full
	})
}

// drainInto reads what is left of a (tee'd) request body, up to max_body_bytes.
func (app *App) drainInto(body io.Reader) {
	if body != nil && body != http.NoBody {
		io.Copy(io.Discard, io.LimitReader(body, app.cfg.MaxBodyBytes))
	}
}

// quickCapture records a request fluid-config answered itself (auth failures,
// denials), without reading its body.
func (app *App) quickCapture(r *http.Request, status int, respHeaders http.Header, respBody string) *Capture {
	c := app.newCapture()
	if c == nil {
		return nil
	}
	target := r.URL.RequestURI()
	if r.Method == http.MethodConnect {
		target = r.Host
	} else if r.URL.IsAbs() {
		target = r.URL.String()
	}
	c.SetRequest(requestHeaders(r.Method, target, r.Host, r.Header), nil)
	if r.Method != http.MethodConnect {
		app.drainInto(c.TeeRequestBody(r.Body))
	}
	c.SetResponse(responseHeaders(status, respHeaders), []byte(respBody))
	c.Finish()
	return c
}
