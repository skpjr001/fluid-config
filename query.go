package main

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// recordQuery filters, searches, sorts and pages an API's records in memory.
type recordQuery struct {
	filters []filter
	search  string // lower-cased
	sorts   []sortKey
	offset  int
	limit   int
	fields  []string
}

type filter struct {
	field, op string
	vals      []string
}

type sortKey struct {
	field string
	desc  bool
}

var filterOps = []string{"eq", "ne", "gt", "gte", "lt", "lte", "like", "in", "exists"}

// reservedParams are list parameters, not field filters.
var reservedParams = map[string]bool{"q": true, "sort": true, "page": true, "limit": true, "fields": true, "offset": true, "format": true, "tz": true}

// parsePublicQuery reads the public API syntax:
//
//	?exchange=NSE  ?date[gte]=2026-03-01  ?exchange[in]=BSE,MCX  ?q=diwali
//	?sort=-date,exchange  ?page=2&limit=50  ?fields=date,reason
func parsePublicQuery(q url.Values, maxLimit int) (recordQuery, error) {
	rq := recordQuery{limit: 50, search: strings.ToLower(q.Get("q"))}
	for k, vals := range q {
		if reservedParams[k] {
			continue
		}
		field, op := k, "eq"
		if i := strings.IndexByte(k, '['); i > 0 && strings.HasSuffix(k, "]") {
			field, op = k[:i], k[i+1:len(k)-1]
		}
		if !contains(filterOps, op) {
			return rq, fmt.Errorf("unknown operator %q (%s)", op, strings.Join(filterOps, " "))
		}
		for _, v := range vals {
			f := filter{field: field, op: op, vals: []string{v}}
			if op == "in" {
				f.vals = strings.Split(v, ",")
			}
			rq.filters = append(rq.filters, f)
		}
	}
	page := 1
	var err error
	if v := q.Get("limit"); v != "" {
		if rq.limit, err = strconv.Atoi(v); err != nil || rq.limit < 1 {
			return rq, fmt.Errorf("limit must be a positive integer")
		}
	}
	rq.limit = min(rq.limit, maxLimit)
	if v := q.Get("page"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 1 {
			return rq, fmt.Errorf("page must be a positive integer")
		}
	}
	rq.offset = (page - 1) * rq.limit
	for _, k := range strings.Split(q.Get("sort"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			rq.sorts = append(rq.sorts, sortKey{strings.TrimPrefix(k, "-"), strings.HasPrefix(k, "-")})
		}
	}
	if f := q.Get("fields"); f != "" {
		rq.fields = strings.Split(f, ",")
	}
	return rq, nil
}

// parseConsoleQuery reads the console syntax (same as the logs tables):
//
//	f.<field>=text      contains (case-insensitive); "=x" exact; "!x" not equal
//	f.<field>=>=10      also >, <=, <; "a..b" is an inclusive range (numbers or dates)
//	sort=field:desc,other:asc  offset=0  limit=50  q=search
func parseConsoleQuery(q url.Values, maxLimit int) (recordQuery, error) {
	rq := recordQuery{limit: 50, search: strings.ToLower(q.Get("q"))}
	for key, vals := range q {
		field, ok := strings.CutPrefix(key, "f.")
		if !ok {
			continue
		}
		for _, v := range vals {
			if v = strings.TrimSpace(v); v == "" {
				continue
			}
			rq.filters = append(rq.filters, consoleFilter(field, v)...)
		}
	}
	var err error
	if v := q.Get("limit"); v != "" {
		if rq.limit, err = strconv.Atoi(v); err != nil || rq.limit < 1 {
			return rq, fmt.Errorf("limit must be a positive integer")
		}
	}
	rq.limit = min(rq.limit, maxLimit)
	if v := q.Get("offset"); v != "" {
		if rq.offset, err = strconv.Atoi(v); err != nil || rq.offset < 0 {
			return rq, fmt.Errorf("offset must be a non-negative number")
		}
	}
	for _, part := range strings.Split(q.Get("sort"), ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		col, dir, _ := strings.Cut(part, ":")
		switch strings.ToLower(dir) {
		case "", "asc":
			rq.sorts = append(rq.sorts, sortKey{col, false})
		case "desc":
			rq.sorts = append(rq.sorts, sortKey{col, true})
		default:
			return rq, fmt.Errorf("sort direction must be asc or desc")
		}
	}
	return rq, nil
}

func consoleFilter(field, v string) []filter {
	one := func(op, x string) []filter { return []filter{{field: field, op: op, vals: []string{x}}} }
	for _, p := range []struct{ prefix, op string }{{">=", "gte"}, {"<=", "lte"}, {">", "gt"}, {"<", "lt"}, {"=", "eq"}, {"!", "ne"}} {
		if rest, ok := strings.CutPrefix(v, p.prefix); ok {
			return one(p.op, rest)
		}
	}
	if a, b, ok := strings.Cut(v, ".."); ok {
		var out []filter
		if a != "" {
			out = append(out, one("gte", a)...)
		}
		if b != "" {
			out = append(out, one("lte", b)...)
		}
		return out
	}
	return one("like", v)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func getPath(row map[string]any, path string) (any, bool) {
	var cur any = row
	for _, p := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// compare: numeric when both sides parse as numbers, else string (so ISO
// dates sort correctly).
func compare(a, b string) int {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea == nil && eb == nil {
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	}
	return strings.Compare(a, b)
}

func (f filter) match(row map[string]any) bool {
	v, ok := getPath(row, f.field)
	if f.op == "exists" {
		return ok == (f.vals[0] != "false")
	}
	if !ok {
		return f.op == "ne"
	}
	sv := toString(v)
	switch f.op {
	case "eq":
		return compare(sv, f.vals[0]) == 0
	case "ne":
		return compare(sv, f.vals[0]) != 0
	case "gt":
		return compare(sv, f.vals[0]) > 0
	case "gte":
		return compare(sv, f.vals[0]) >= 0
	case "lt":
		return compare(sv, f.vals[0]) < 0
	case "lte":
		return compare(sv, f.vals[0]) <= 0
	case "like":
		return strings.Contains(strings.ToLower(sv), strings.ToLower(f.vals[0]))
	case "in":
		for _, x := range f.vals {
			if compare(sv, strings.TrimSpace(x)) == 0 {
				return true
			}
		}
	}
	return false
}

func matchQ(row map[string]any, q string) bool {
	for _, v := range row {
		if strings.Contains(strings.ToLower(toString(v)), q) {
			return true
		}
	}
	return false
}

func project(row map[string]any, fields []string, idField string) map[string]any {
	if len(fields) == 0 {
		return row
	}
	out := map[string]any{idField: row[idField]}
	for _, f := range fields {
		if v, ok := getPath(row, f); ok {
			out[f] = v
		}
	}
	return out
}

// run returns every matching row (sorted) and the requested page of them.
func (rq recordQuery) run(data []map[string]any, idField string) (all, page []map[string]any) {
	all = make([]map[string]any, 0)
rows:
	for _, row := range data {
		for _, f := range rq.filters {
			if !f.match(row) {
				continue rows
			}
		}
		if rq.search != "" && !matchQ(row, rq.search) {
			continue
		}
		all = append(all, row)
	}
	if len(rq.sorts) > 0 {
		sort.SliceStable(all, func(i, j int) bool {
			for _, k := range rq.sorts {
				a, aok := getPath(all[i], k.field)
				b, bok := getPath(all[j], k.field)
				if aok != bok {
					return aok // missing values last
				}
				c := compare(toString(a), toString(b))
				if c == 0 {
					continue
				}
				if k.desc {
					return c > 0
				}
				return c < 0
			}
			return false
		})
	}
	from := min(rq.offset, len(all))
	to := min(from+rq.limit, len(all))
	page = make([]map[string]any, 0, to-from)
	for _, row := range all[from:to] {
		page = append(page, project(row, rq.fields, idField))
	}
	return all, page
}
