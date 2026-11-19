package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

var levelByName = map[string]slog.Level{
	"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
}

// LogRow is one stored log record. Well-known attributes get columns so they
// can be filtered and grouped; everything else goes into Attrs.
type LogRow struct {
	ID         int64          `json:"id"`
	TS         time.Time      `json:"ts"`
	Level      string         `json:"level"`
	Kind       string         `json:"kind"`
	Direction  string         `json:"direction,omitempty"` // inbound | outbound
	Msg        string         `json:"msg"`
	Client     string         `json:"client,omitempty"`
	Method     string         `json:"method,omitempty"`
	Host       string         `json:"host,omitempty"`
	URL        string         `json:"url,omitempty"`
	Status     int            `json:"status,omitempty"`
	BytesIn    int64          `json:"bytes_in,omitempty"`
	BytesOut   int64          `json:"bytes_out,omitempty"`
	DurationMs int64          `json:"duration_ms,omitempty"`
	SourceIP   string         `json:"source_ip,omitempty"`
	Route      string         `json:"route,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	Attrs      map[string]any `json:"attrs,omitempty"`
	HasPayload bool           `json:"has_payload,omitempty"` // request/response captured: GET /api/logs/{id}

	capture *Capture // written with the row once the exchange finishes
}

// PayloadPart is one side of a captured exchange.
type PayloadPart struct {
	Headers    http.Header `json:"headers"`
	Body       string      `json:"body,omitempty"`        // UTF-8 bodies
	BodyBase64 string      `json:"body_base64,omitempty"` // binary bodies
	Size       int64       `json:"size"`                  // full size on the wire
	Stored     int64       `json:"stored"`                // bytes kept (capture_max_body_bytes)
	Truncated  bool        `json:"truncated,omitempty"`
}

type LogPayload struct {
	Request  *PayloadPart `json:"request,omitempty"`  // RQH + RQB
	Response *PayloadPart `json:"response,omitempty"` // RSH + RSB
}

// LogSink batches rows into SQLite on a background goroutine so request
// paths never wait on disk. If the queue is full rows are dropped and counted.
type LogSink struct {
	db       *DB
	ch       chan *LogRow
	dropped  atomic.Int64
	minLevel slog.Level
	hub      *Hub
	done     chan struct{}
	mu       sync.RWMutex // guards closed vs. sends on ch
	closed   bool
}

func NewLogSink(db *DB, minLevel slog.Level, hub *Hub) *LogSink {
	s := &LogSink{db: db, ch: make(chan *LogRow, 16384), minLevel: minLevel, hub: hub, done: make(chan struct{})}
	go s.run()
	return s
}

func (s *LogSink) enqueue(r *LogRow) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed { // late logs during shutdown still reach stdout
		return
	}
	select {
	case s.ch <- r:
	default:
		s.dropped.Add(1)
	}
}

func (s *LogSink) Dropped() int64 { return s.dropped.Load() }

// Close flushes queued rows and stops the writer.
func (s *LogSink) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	s.mu.Unlock()
	<-s.done
}

func (s *LogSink) run() {
	defer close(s.done)
	batch := make([]*LogRow, 0, 256)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insert(batch); err != nil {
			s.dropped.Add(int64(len(batch)))
			fmt.Printf(`{"level":"ERROR","msg":"log store write failed","err":%q}`+"\n", err.Error())
		}
		batch = batch[:0]
	}
	for {
		select {
		case r, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) >= 256 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func (s *LogSink) insert(rows []*LogRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	st, err := tx.Prepare(`INSERT INTO logs (ts, level, kind, direction, msg, client, method, host, url, status,
		bytes_in, bytes_out, duration_ms, source_ip, route, trace_id, attrs,
		rq_headers, rq_body, rq_size, rs_headers, rs_body, rs_size) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer st.Close()
	for _, r := range rows {
		var attrs any
		if len(r.Attrs) > 0 {
			b, _ := json.Marshal(r.Attrs)
			attrs = string(b)
		}
		var rqh, rqb, rqs, rsh, rsb, rss any
		if r.capture != nil {
			c := r.capture.snapshot()
			if c.reqHeaders != nil {
				b, _ := json.Marshal(c.reqHeaders)
				rqh, rqb, rqs = string(b), c.reqBody, c.reqSize
			}
			if c.respHeaders != nil {
				b, _ := json.Marshal(c.respHeaders)
				rsh, rsb, rss = string(b), c.respBody, c.respSize
			}
			r.HasPayload = rqh != nil || rsh != nil
		}
		res, err := st.Exec(r.TS.UnixMilli(), r.Level, r.Kind, nullStr(r.Direction), r.Msg, nullStr(r.Client), nullStr(r.Method),
			nullStr(r.Host), nullStr(r.URL), nullInt(int64(r.Status)), nullInt(r.BytesIn), nullInt(r.BytesOut),
			nullInt(r.DurationMs), nullStr(r.SourceIP), nullStr(r.Route), nullStr(r.TraceID), attrs,
			rqh, rqb, rqs, rsh, rsb, rss)
		if err != nil {
			tx.Rollback()
			return err
		}
		r.ID, _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.hub != nil {
		for _, r := range rows {
			s.hub.Publish("logs", r)
		}
	}
	return nil
}

// ---------- slog handler ----------

// teeHandler writes every record to next (stdout JSON) and stores records at
// or above the sink's level in SQLite.
type teeHandler struct {
	next  slog.Handler
	sink  *LogSink
	attrs []slog.Attr
	group string
}

func NewTeeHandler(next slog.Handler, sink *LogSink) slog.Handler {
	return &teeHandler{next: next, sink: sink}
}

func (h *teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l) || (h.sink != nil && l >= h.sink.minLevel)
}

func (h *teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(as)
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &c
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	c.group = name
	return &c
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h.next.Enabled(ctx, r.Level) {
		err = h.next.Handle(ctx, r)
	}
	if h.sink == nil || r.Level < h.sink.minLevel {
		return err
	}
	row := &LogRow{TS: r.Time, Level: strings.ToLower(r.Level.String()), Kind: "app", Msg: r.Message}
	if row.TS.IsZero() {
		row.TS = time.Now()
	}
	set := func(a slog.Attr) {
		if a.Key == "capture" {
			if c, ok := a.Value.Any().(*Capture); ok && c != nil {
				row.capture = c
			}
			return
		}
		v := a.Value.Resolve()
		switch a.Key {
		case "kind":
			row.Kind = v.String()
		case "direction":
			row.Direction = v.String()
		case "client":
			row.Client = v.String()
		case "method":
			row.Method = v.String()
		case "host":
			row.Host = v.String()
		case "url", "path", "dest":
			if row.URL == "" {
				row.URL = v.String()
			}
		case "status":
			row.Status = int(asInt(v))
		case "bytes_in":
			row.BytesIn = asInt(v)
		case "bytes_out":
			row.BytesOut = asInt(v)
		case "ms":
			row.DurationMs = asInt(v)
		case "source_ip", "client_ip":
			row.SourceIP = v.String()
		case "route", "api":
			row.Route = v.String()
		case "trace_id":
			row.TraceID = v.String()
		default:
			addAttr(row, a.Key, v)
		}
	}
	for _, a := range h.attrs {
		set(a)
	}
	r.Attrs(func(a slog.Attr) bool { set(a); return true })
	if row.capture != nil {
		row.capture.whenDone(func() { h.sink.enqueue(row) }) // stored once the body has been fully seen
	} else {
		h.sink.enqueue(row)
	}
	return err
}

func asInt(v slog.Value) int64 {
	switch v.Kind() {
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return int64(v.Uint64())
	case slog.KindFloat64:
		return int64(v.Float64())
	case slog.KindDuration:
		return v.Duration().Milliseconds()
	}
	n, _ := strconv.ParseInt(v.String(), 10, 64)
	return n
}

func addAttr(row *LogRow, k string, v slog.Value) {
	if row.Attrs == nil {
		row.Attrs = map[string]any{}
	}
	row.Attrs[k] = v.Any()
	if e, ok := v.Any().(error); ok {
		row.Attrs[k] = e.Error()
	}
}

// ---------- queries ----------

// LogQuery filters logs. Empty fields are ignored.
type LogQuery struct {
	Kinds     []string
	Direction string // inbound | outbound
	Level     string // minimum level
	Client    string
	Route     string
	Host      string
	SourceIP  string
	Status    string // "404", "4xx", "5xx", "error" (>=400)
	Since     time.Time
	Until     time.Time
	Text      string // substring of msg, url or attrs
	BeforeID  int64  // cursor: only rows with id < BeforeID
	Limit     int
	List      ListParams // column filters, sort, offset (API)
}

var levelOrder = []string{"debug", "info", "warn", "error"}

func (q LogQuery) where() (string, []any, error) {
	conds, args := append([]string(nil), q.List.Conds...), append([]any(nil), q.List.Args...)
	add := func(c string, a ...any) { conds = append(conds, c); args = append(args, a...) }
	if len(q.Kinds) > 0 {
		add("kind IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.Kinds)), ",")+")", toAny(q.Kinds)...)
	}
	if q.Level != "" {
		lv := strings.ToLower(q.Level)
		idx := -1
		for i, l := range levelOrder {
			if l == lv {
				idx = i
			}
		}
		if idx < 0 {
			return "", nil, fmt.Errorf("level must be one of %v", levelOrder)
		}
		add("level IN ("+strings.TrimSuffix(strings.Repeat("?,", len(levelOrder)-idx), ",")+")", toAny(levelOrder[idx:])...)
	}
	switch q.Direction {
	case "":
	case "inbound", "outbound":
		add("direction = ?", q.Direction)
	default:
		return "", nil, fmt.Errorf("direction must be inbound or outbound")
	}
	if q.Client != "" {
		add("client = ?", q.Client)
	}
	if q.Route != "" {
		add("route = ?", q.Route)
	}
	if q.Host != "" {
		add("host = ?", strings.ToLower(q.Host))
	}
	if q.SourceIP != "" {
		add("source_ip = ?", q.SourceIP)
	}
	switch s := strings.ToLower(q.Status); {
	case s == "":
	case s == "error":
		add("status >= 400")
	case len(s) == 3 && strings.HasSuffix(s, "xx") && s[0] >= '1' && s[0] <= '5':
		lo := int(s[0]-'0') * 100
		add("status >= ? AND status < ?", lo, lo+100)
	default:
		n, err := strconv.Atoi(s)
		if err != nil {
			return "", nil, fmt.Errorf("status must be a code, 1xx-5xx or error")
		}
		add("status = ?", n)
	}
	if !q.Since.IsZero() {
		add("ts >= ?", q.Since.UnixMilli())
	}
	if !q.Until.IsZero() {
		add("ts < ?", q.Until.UnixMilli())
	}
	if q.Text != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Text) + "%"
		add(`(msg LIKE ? ESCAPE '\' OR url LIKE ? ESCAPE '\' OR attrs LIKE ? ESCAPE '\')`, like, like, like)
	}
	if q.BeforeID > 0 {
		add("id < ?", q.BeforeID)
	}
	if len(conds) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

func toAny[T any](xs []T) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// GetLogPayload returns the captured request/response of one log row.
func GetLogPayload(ctx context.Context, db *DB, id int64) (*LogPayload, error) {
	var rqh, rsh *string
	var rqb, rsb []byte
	var rqs, rss *int64
	err := db.QueryRowContext(ctx, `SELECT rq_headers, rq_body, rq_size, rs_headers, rs_body, rs_size FROM logs WHERE id = ?`, id).
		Scan(&rqh, &rqb, &rqs, &rsh, &rsb, &rss)
	if err != nil {
		return nil, err
	}
	part := func(h *string, b []byte, n *int64) *PayloadPart {
		if h == nil {
			return nil
		}
		p := &PayloadPart{Headers: http.Header{}, Size: derefInt(n), Stored: int64(len(b))}
		json.Unmarshal([]byte(*h), &p.Headers)
		p.Truncated = p.Size > p.Stored
		if utf8.Valid(b) {
			p.Body = string(b)
		} else {
			p.BodyBase64 = base64.StdEncoding.EncodeToString(b)
		}
		return p
	}
	return &LogPayload{Request: part(rqh, rqb, rqs), Response: part(rsh, rsb, rss)}, nil
}

// CountLogs is the total for the same filters (for paging).
func CountLogs(ctx context.Context, db *DB, q LogQuery) (int64, error) {
	where, args, err := q.where()
	if err != nil {
		return 0, err
	}
	var n int64
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`+where, args...).Scan(&n)
	return n, err
}

// QueryLogs returns rows newest first and the cursor for the next page (0 = end).
func QueryLogs(ctx context.Context, db *DB, q LogQuery) ([]LogRow, int64, error) {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}
	where, args, err := q.where()
	if err != nil {
		return nil, 0, err
	}
	order := q.List.Order
	if order == "" {
		order = "id DESC"
	}
	rows, err := db.QueryContext(ctx, `SELECT id, ts, level, kind, direction, msg, client, method, host, url, status,
		bytes_in, bytes_out, duration_ms, source_ip, route, trace_id, attrs,
		(rq_headers IS NOT NULL OR rs_headers IS NOT NULL) FROM logs`+where+
		` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, q.Limit, q.List.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []LogRow{}
	for rows.Next() {
		var r LogRow
		var ts int64
		var dir, client, method, host, u, src, route, trace, attrs *string
		var status, bin, bout, dur *int64
		if err := rows.Scan(&r.ID, &ts, &r.Level, &r.Kind, &dir, &r.Msg, &client, &method, &host, &u, &status,
			&bin, &bout, &dur, &src, &route, &trace, &attrs, &r.HasPayload); err != nil {
			return nil, 0, err
		}
		r.TS = time.UnixMilli(ts)
		r.Direction = deref(dir)
		r.Client, r.Method, r.Host, r.URL = deref(client), deref(method), deref(host), deref(u)
		r.SourceIP, r.Route, r.TraceID = deref(src), deref(route), deref(trace)
		r.Status, r.BytesIn, r.BytesOut, r.DurationMs = int(derefInt(status)), derefInt(bin), derefInt(bout), derefInt(dur)
		if attrs != nil {
			json.Unmarshal([]byte(*attrs), &r.Attrs)
		}
		out = append(out, r)
	}
	var next int64
	if len(out) == q.Limit {
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

// SummaryRow is one group in a log summary.
type SummaryRow struct {
	Key         string  `json:"key"`
	Count       int64   `json:"count"`
	Errors      int64   `json:"errors"` // status >= 400 or level error
	AvgMs       float64 `json:"avg_ms"`
	BytesOut    int64   `json:"bytes_out"`
	LastSeenISO string  `json:"last_seen"`
}

var summaryColumns = map[string]string{
	"kind": "kind", "level": "level", "client": "client", "route": "route",
	"status": "status", "source_ip": "source_ip", "method": "method",
}

// SummarizeLogs groups matching logs by one column.
func SummarizeLogs(ctx context.Context, db *DB, q LogQuery, by string) ([]SummaryRow, error) {
	col, ok := summaryColumns[by]
	if !ok {
		return nil, fmt.Errorf("by must be one of kind, level, client, route, status, source_ip, method")
	}
	q.BeforeID = 0
	where, args, err := q.where()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT COALESCE(CAST(`+col+` AS TEXT), ''), COUNT(*),
		SUM(CASE WHEN status >= 400 OR level = 'error' THEN 1 ELSE 0 END),
		COALESCE(AVG(duration_ms), 0), COALESCE(SUM(bytes_out), 0), MAX(ts)
		FROM logs`+where+` GROUP BY 1 ORDER BY 2 DESC LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SummaryRow{}
	for rows.Next() {
		var r SummaryRow
		var last int64
		if err := rows.Scan(&r.Key, &r.Count, &r.Errors, &r.AvgMs, &r.BytesOut, &last); err != nil {
			return nil, err
		}
		r.LastSeenISO = time.UnixMilli(last).UTC().Format(time.RFC3339)
		out = append(out, r)
	}
	return out, rows.Err()
}

// parseTimeArg accepts RFC3339 or a duration meaning "that long ago" ("15m", "24h", "7d").
func parseTimeArg(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Now().Add(-time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("bad time %q: use RFC3339 or a duration like 15m, 24h, 7d", s)
}

// Prune deletes request logs and audit rows older than their retention.
func Prune(ctx context.Context, db *DB, logs, audit time.Duration) (int64, error) {
	var total int64
	for _, p := range []struct {
		table string
		keep  time.Duration
	}{{"logs", logs}, {"audit", audit}} {
		res, err := db.ExecContext(ctx, `DELETE FROM `+p.table+` WHERE ts < ?`, time.Now().Add(-p.keep).UnixMilli())
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
