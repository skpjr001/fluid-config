package server

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Server-side paging, per-column filters and multi-column sorting for the
// logs and audit lists (stored in SQLite).
//
//	f.<column>=<value>   filter (several columns combine with AND)
//	                     text:   case-insensitive "contains"; "=exact" for exact
//	                     number: 500 | 5xx | >=400 | <100 | 200-299
//	                     time:   RFC3339 or local "2026-11-17T10:00" range "a..b", or ">=a" / "<b"
//	sort=col:desc,col2:asc
//	offset=0&limit=50
//
// Columns are whitelisted per list, so user input never becomes SQL.

type colKind int

const (
	colText colKind = iota
	colNum
	colTime // stored as unix milliseconds
)

type colSpec struct {
	sql  string
	kind colKind
}

type listSpec struct {
	cols        map[string]colSpec
	defaultSort string // SQL ORDER BY when no sort is given
	tiebreak    string // appended so paging is stable, e.g. "id DESC"
}

// ListParams is what a request asks for beyond the list's own filters.
type ListParams struct {
	Conds  []string
	Args   []any
	Order  string // SQL ORDER BY clause (validated)
	Offset int
	Sorts  []string // normalized "col:dir" for echoing back
}

const maxPageSize = 500

func (s listSpec) parse(q url.Values) (ListParams, error) {
	var p ListParams
	for key, vals := range q {
		col, ok := strings.CutPrefix(key, "f.")
		if !ok {
			continue
		}
		spec, ok := s.cols[col]
		if !ok {
			return p, fmt.Errorf("cannot filter by %q", col)
		}
		for _, v := range vals {
			if v = strings.TrimSpace(v); v == "" {
				continue
			}
			c, a, err := filterCond(spec, v)
			if err != nil {
				return p, fmt.Errorf("f.%s: %w", col, err)
			}
			p.Conds, p.Args = append(p.Conds, c), append(p.Args, a...)
		}
	}
	var order []string
	for _, part := range strings.Split(q.Get("sort"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		col, dir, _ := strings.Cut(part, ":")
		spec, ok := s.cols[col]
		if !ok {
			return p, fmt.Errorf("cannot sort by %q", col)
		}
		switch dir = strings.ToLower(dir); dir {
		case "", "asc":
			dir = "asc"
		case "desc":
		default:
			return p, fmt.Errorf("sort direction must be asc or desc")
		}
		order = append(order, spec.sql+" "+strings.ToUpper(dir))
		p.Sorts = append(p.Sorts, col+":"+dir)
	}
	if len(order) == 0 {
		p.Order = s.defaultSort
	} else {
		p.Order = strings.Join(append(order, s.tiebreak), ", ")
	}
	if o := q.Get("offset"); o != "" {
		n, err := strconv.Atoi(o)
		if err != nil || n < 0 {
			return p, fmt.Errorf("offset must be a non-negative number")
		}
		p.Offset = n
	}
	return p, nil
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func filterCond(c colSpec, v string) (string, []any, error) {
	switch c.kind {
	case colText:
		if exact, ok := strings.CutPrefix(v, "="); ok {
			return c.sql + " = ?", []any{exact}, nil
		}
		return "LOWER(COALESCE(" + c.sql + `,'')) LIKE ? ESCAPE '\'`, []any{"%" + strings.ToLower(likeEscaper.Replace(v)) + "%"}, nil
	case colNum:
		return numCond(c.sql, v)
	default:
		return timeCond(c.sql, v)
	}
}

func numCond(col, v string) (string, []any, error) {
	v = strings.ReplaceAll(v, " ", "")
	bad := fmt.Errorf("%q: use a number, 5xx, >=400, <100 or 200-299", v)
	if len(v) == 3 && strings.HasSuffix(strings.ToLower(v), "xx") && v[0] >= '1' && v[0] <= '9' {
		lo := int64(v[0]-'0') * 100
		return col + " >= ? AND " + col + " < ?", []any{lo, lo + 100}, nil
	}
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if rest, ok := strings.CutPrefix(v, op); ok {
			n, err := strconv.ParseInt(rest, 10, 64)
			if err != nil {
				return "", nil, bad
			}
			return col + " " + op + " ?", []any{n}, nil
		}
	}
	if a, b, ok := strings.Cut(v, "-"); ok && a != "" {
		lo, err1 := strconv.ParseInt(a, 10, 64)
		hi, err2 := strconv.ParseInt(b, 10, 64)
		if err1 != nil || err2 != nil {
			return "", nil, bad
		}
		return col + " BETWEEN ? AND ?", []any{lo, hi}, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return "", nil, bad
	}
	return col + " = ?", []any{n}, nil
}

// parseInstant accepts RFC3339 (the UI sends local times converted to UTC)
// or a relative duration like "1h" meaning "that long ago".
func parseInstant(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := parseTimeArg(s); err == nil && !t.IsZero() {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q: use RFC3339 (2026-11-17T10:00:00Z) or a duration like 1h", s)
}

func timeCond(col, v string) (string, []any, error) {
	if a, b, ok := strings.Cut(v, ".."); ok {
		var conds []string
		var args []any
		if a != "" {
			t, err := parseInstant(a)
			if err != nil {
				return "", nil, err
			}
			conds, args = append(conds, col+" >= ?"), append(args, t.UnixMilli())
		}
		if b != "" {
			t, err := parseInstant(b)
			if err != nil {
				return "", nil, err
			}
			conds, args = append(conds, col+" < ?"), append(args, t.UnixMilli())
		}
		if len(conds) == 0 {
			return "1=1", nil, nil
		}
		return strings.Join(conds, " AND "), args, nil
	}
	for _, op := range []string{">=", "<=", ">", "<"} {
		if rest, ok := strings.CutPrefix(v, op); ok {
			t, err := parseInstant(rest)
			if err != nil {
				return "", nil, err
			}
			return col + " " + op + " ?", []any{t.UnixMilli()}, nil
		}
	}
	t, err := parseInstant(v)
	if err != nil {
		return "", nil, err
	}
	return col + " >= ?", []any{t.UnixMilli()}, nil
}

// ---------- the lists ----------

var logListSpec = listSpec{
	cols: map[string]colSpec{
		"id": {"id", colNum}, "ts": {"ts", colTime}, "level": {"level", colText}, "kind": {"kind", colText},
		"direction": {"direction", colText}, "msg": {"msg", colText}, "client": {"client", colText},
		"method": {"method", colText}, "host": {"host", colText}, "url": {"url", colText}, "status": {"status", colNum},
		"bytes_in": {"bytes_in", colNum}, "bytes_out": {"bytes_out", colNum}, "duration_ms": {"duration_ms", colNum},
		"source_ip": {"source_ip", colText}, "route": {"route", colText}, "trace_id": {"trace_id", colText},
	},
	defaultSort: "id DESC", tiebreak: "id DESC",
}

var auditListSpec = listSpec{
	cols: map[string]colSpec{
		"id": {"id", colNum}, "ts": {"ts", colTime}, "api": {"api", colText}, "record_id": {"record_id", colText},
		"action": {"action", colText}, "actor": {"actor", colText}, "via": {"via", colText},
		"source_ip": {"source_ip", colText}, "request_id": {"request_id", colText},
	},
	defaultSort: "id DESC", tiebreak: "id DESC",
}

func clampLimit(n, def int) int {
	if n <= 0 {
		return def
	}
	return min(n, maxPageSize)
}
