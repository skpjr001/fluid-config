package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/skpjr001/fluid-config/internal/database"
	"github.com/skpjr001/fluid-config/internal/hub"
)

// AuditEntry is one change to an API: a record created, replaced, patched,
// deleted or reverted, or the API's contract created, changed or deleted.
// Hand edits of the files are recorded too (via "file", actor "file").
type AuditEntry struct {
	ID        int64           `json:"id"`
	TS        time.Time       `json:"ts"`
	API       string          `json:"api"`
	RecordID  string          `json:"record_id,omitempty"`
	Action    string          `json:"action"`
	Actor     string          `json:"actor"`
	Via       string          `json:"via"` // api | console | file
	SourceIP  string          `json:"source_ip,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
}

// Who did a change, for audit entries.
type actor struct {
	name, via, ip, requestID string
}

func (a actor) entry(api, id, action string, before, after any) AuditEntry {
	return AuditEntry{TS: time.Now(), API: api, RecordID: id, Action: action, Actor: a.name, Via: a.via,
		SourceIP: a.ip, RequestID: a.requestID, Before: rawJSON(before), After: rawJSON(after)}
}

func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok && m == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// maskedContract is an API's contract for the audit trail and the console:
// no data, and literal credentials replaced by Mask (env: and sha256:
// references are not secrets and stay readable).
func maskedContract(s *APISpec) *APISpec {
	c := s.contract()
	for i, t := range c.Auth.Tokens {
		if _, v := splitTokenName(t, i); !isSecretRef(v) {
			c.Auth.Tokens[i] = Mask // literal tokens are never shown
		}
	}
	for u, p := range c.Auth.Users {
		if !isSecretRef(p) {
			c.Auth.Users[u] = Mask
		}
	}
	return c
}

func isSecretRef(v string) bool {
	return strings.HasPrefix(v, "env:") || strings.HasPrefix(v, "sha256:")
}

// AuditLog writes audit rows synchronously: a change is acknowledged only
// after its history row exists (the file stays the source of truth).
type AuditLog struct {
	db  *database.DB
	hub *hub.Hub
}

func (l *AuditLog) Record(ctx context.Context, es ...AuditEntry) {
	if len(es) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("audit write failed", "kind", "system", "err", err)
		return
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT INTO audit (ts, api, record_id, action, actor, via, source_ip, request_id, before, after)
		VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		slog.Error("audit write failed", "kind", "system", "err", err)
		return
	}
	defer st.Close()
	str := func(b json.RawMessage) any {
		if b == nil {
			return nil
		}
		return string(b)
	}
	for i := range es {
		e := &es[i]
		res, err := st.ExecContext(ctx, e.TS.UnixMilli(), e.API, nullStr(e.RecordID), e.Action, e.Actor, e.Via,
			nullStr(e.SourceIP), nullStr(e.RequestID), str(e.Before), str(e.After))
		if err != nil {
			slog.Error("audit write failed", "kind", "system", "err", err)
			return
		}
		e.ID, _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		slog.Error("audit write failed", "kind", "system", "err", err)
		return
	}
	for _, e := range es {
		l.hub.Publish("audit", e)
	}
}

// AuditQuery filters the history; List carries column filters, sort, offset.
type AuditQuery struct {
	API      string
	RecordID string
	Text     string
	Limit    int
	List     ListParams
}

func (q AuditQuery) where() (string, []any) {
	conds, args := append([]string(nil), q.List.Conds...), append([]any(nil), q.List.Args...)
	if q.API != "" {
		conds, args = append(conds, "api = ?"), append(args, q.API)
	}
	if q.RecordID != "" {
		conds, args = append(conds, "record_id = ?"), append(args, q.RecordID)
	}
	if q.Text != "" {
		like := "%" + likeEscaper.Replace(q.Text) + "%"
		conds = append(conds, `(before LIKE ? ESCAPE '\' OR after LIKE ? ESCAPE '\' OR record_id LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (l *AuditLog) Count(ctx context.Context, q AuditQuery) (int64, error) {
	where, args := q.where()
	var n int64
	err := l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit`+where, args...).Scan(&n)
	return n, err
}

func (l *AuditLog) List(ctx context.Context, q AuditQuery) ([]AuditEntry, error) {
	where, args := q.where()
	order := q.List.Order
	if order == "" {
		order = "id DESC"
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	rows, err := l.db.QueryContext(ctx, `SELECT id, ts, api, record_id, action, actor, via, source_ip, request_id, before, after
		FROM audit`+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, q.Limit, q.List.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (l *AuditLog) Get(ctx context.Context, id int64) (AuditEntry, error) {
	row := l.db.QueryRowContext(ctx, `SELECT id, ts, api, record_id, action, actor, via, source_ip, request_id, before, after
		FROM audit WHERE id = ?`, id)
	return scanAudit(row)
}

type scanner interface{ Scan(...any) error }

func scanAudit(r scanner) (AuditEntry, error) {
	var e AuditEntry
	var ts int64
	var rid, ip, reqID, before, after sql.NullString
	if err := r.Scan(&e.ID, &ts, &e.API, &rid, &e.Action, &e.Actor, &e.Via, &ip, &reqID, &before, &after); err != nil {
		return e, err
	}
	e.TS = time.UnixMilli(ts)
	e.RecordID, e.SourceIP, e.RequestID = rid.String, ip.String, reqID.String
	if before.Valid {
		e.Before = json.RawMessage(before.String)
	}
	if after.Valid {
		e.After = json.RawMessage(after.String)
	}
	return e, nil
}

// Counts of changes per API since a time (for the overview).
func (l *AuditLog) CountsSince(ctx context.Context, since time.Time) (map[string]int64, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT api, COUNT(*) FROM audit WHERE ts >= ? GROUP BY api`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// maxFileChanges caps the per-record rows recorded for one hand edit.
const maxFileChanges = 5000

// fileChanges describes what hand edits changed between two loads: APIs
// added, removed or with a new contract, and every record created, replaced
// or deleted. Writes made through fluid-config are already in the snapshot,
// so re-reading them produces no entries.
func fileChanges(old, next *snapshot) []AuditEntry {
	who := actor{name: "file", via: "file"}
	var out []AuditEntry
	for _, k := range slices.Sorted(maps.Keys(old.apis)) {
		if next.apis[k] == nil {
			out = append(out, who.entry(k, "", "api_delete", maskedContract(old.apis[k]), nil))
		}
	}
	rows := 0
	for _, k := range slices.Sorted(maps.Keys(next.apis)) {
		n, o := next.apis[k], old.apis[k]
		if o == nil {
			out = append(out, who.entry(k, "", "api_create", nil, maskedContract(n)))
			continue
		}
		if o == n {
			continue
		}
		before, after := rawJSON(maskedContract(o)), rawJSON(maskedContract(n))
		if string(before) != string(after) {
			out = append(out, who.entry(k, "", "api_update", json.RawMessage(before), json.RawMessage(after)))
		}
		if o.etag == n.etag {
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(o.index)) {
			if _, ok := n.index[id]; !ok && rows < maxFileChanges {
				out = append(out, who.entry(k, id, "delete", o.Data[o.index[id]], nil))
				rows++
			}
		}
		for _, id := range slices.Sorted(maps.Keys(n.index)) {
			if rows >= maxFileChanges {
				break
			}
			nr := n.Data[n.index[id]]
			i, ok := o.index[id]
			switch {
			case !ok:
				out = append(out, who.entry(k, id, "create", nil, nr))
				rows++
			case string(rawJSON(o.Data[i])) != string(rawJSON(nr)):
				out = append(out, who.entry(k, id, "replace", o.Data[i], nr))
				rows++
			}
		}
	}
	return out
}
