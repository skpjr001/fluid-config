package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
)

// Record operations shared by the public API and the console. Each one runs
// under the store's write lock, persists the file, records the change in the
// audit trail and drops the API's cached responses.

// checkMatch implements If-Match: "" (no precondition), "*" (must exist),
// or the record's ETag as returned by GET.
func checkMatch(ifMatch string, cur map[string]any, exists bool) error {
	if ifMatch == "" {
		return nil
	}
	if !exists {
		return &httpErr{412, "precondition failed: the record does not exist"}
	}
	if ifMatch == "*" {
		return nil
	}
	tag := recordETag(cur)
	for _, t := range strings.Split(ifMatch, ",") {
		if t = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "W/")); t == tag {
			return nil
		}
	}
	return &httpErr{412, "precondition failed: the record was changed by someone else; reload it and try again"}
}

func validID(id string) error {
	if id == "" || strings.ContainsAny(id, "/?#") || strings.HasPrefix(id, "_") || len(id) > 256 {
		return &httpErr{400, "invalid id: must be non-empty, without / ? #, not starting with _, at most 256 characters"}
	}
	return nil
}

func nextID(spec *APISpec) any {
	max := new(big.Int)
	numeric := len(spec.Data) > 0
	for _, row := range spec.Data {
		n, ok := new(big.Int).SetString(toString(row[spec.IDField]), 10)
		if !ok {
			numeric = false
			break
		}
		if n.Cmp(max) > 0 {
			max = n
		}
	}
	if numeric {
		return json.Number(max.Add(max, big.NewInt(1)).String())
	}
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func unprocessable(err error) error { return &httpErr{422, err.Error()} }

func (app *App) afterWrite(ctx context.Context, key string, es ...AuditEntry) {
	app.cache.Invalidate(key)
	app.audit.Record(ctx, es...)
	app.metrics.Add("fluid_record_changes_total", int64(len(es)), "api", key)
}

func (app *App) createRecord(ctx context.Context, key string, body map[string]any, who actor) (map[string]any, *APISpec, error) {
	n, err := app.store.Mutate(key, func(cur *APISpec) ([]map[string]any, error) {
		if v, has := body[cur.IDField]; !has || v == nil || v == "" {
			body[cur.IDField] = nextID(cur)
		}
		id := toString(body[cur.IDField])
		if err := validID(id); err != nil {
			return nil, err
		}
		if _, dup := cur.index[id]; dup {
			return nil, &httpErr{409, "a record with " + cur.IDField + " " + id + " already exists"}
		}
		if cur.MaxRecords > 0 && len(cur.Data) >= cur.MaxRecords {
			return nil, &httpErr{409, fmt.Sprintf("this API holds at most %d records (max_records)", cur.MaxRecords)}
		}
		if err := cur.validate(body, false); err != nil {
			return nil, unprocessable(err)
		}
		return append(slices.Clip(cur.Data), body), nil
	})
	if err != nil {
		return nil, nil, err
	}
	app.afterWrite(ctx, key, who.entry(key, toString(body[n.IDField]), "create", nil, body))
	return body, n, nil
}

// replaceRecord is PUT: replace, or create when missing (created = true).
func (app *App) replaceRecord(ctx context.Context, key, id string, body map[string]any, ifMatch string, who actor) (map[string]any, bool, *APISpec, error) {
	var before map[string]any
	created := false
	n, err := app.store.Mutate(key, func(cur *APISpec) ([]map[string]any, error) {
		if err := validID(id); err != nil {
			return nil, err
		}
		old, exists := cur.record(id)
		if err := checkMatch(ifMatch, old, exists); err != nil {
			return nil, err
		}
		if v, has := body[cur.IDField]; has && toString(v) != id {
			return nil, &httpErr{400, cur.IDField + " in the body does not match the URL"}
		}
		if _, has := body[cur.IDField]; !has {
			if exists {
				body[cur.IDField] = old[cur.IDField] // keep the original type (number or string)
			} else {
				body[cur.IDField] = id
			}
		}
		if err := cur.validate(body, false); err != nil {
			return nil, unprocessable(err)
		}
		data := slices.Clone(cur.Data)
		if exists {
			before = old
			data[cur.index[id]] = body
			return data, nil
		}
		if cur.MaxRecords > 0 && len(cur.Data) >= cur.MaxRecords {
			return nil, &httpErr{409, fmt.Sprintf("this API holds at most %d records (max_records)", cur.MaxRecords)}
		}
		created = true
		return append(data, body), nil
	})
	if err != nil {
		return nil, false, nil, err
	}
	action := "replace"
	if created {
		action = "create"
	}
	app.afterWrite(ctx, key, who.entry(key, id, action, before, body))
	return body, created, n, nil
}

// patchRecord is PATCH with JSON Merge Patch semantics: null removes a field.
func (app *App) patchRecord(ctx context.Context, key, id string, patch map[string]any, ifMatch string, who actor) (map[string]any, *APISpec, error) {
	var before, merged map[string]any
	n, err := app.store.Mutate(key, func(cur *APISpec) ([]map[string]any, error) {
		old, exists := cur.record(id)
		if !exists {
			return nil, &httpErr{404, "record not found"}
		}
		if err := checkMatch(ifMatch, old, true); err != nil {
			return nil, err
		}
		if v, has := patch[cur.IDField]; has && toString(v) != id {
			return nil, &httpErr{400, cur.IDField + " cannot be changed"}
		}
		merged = mergePatch(old, patch)
		if err := cur.validate(merged, false); err != nil {
			return nil, unprocessable(err)
		}
		before = old
		data := slices.Clone(cur.Data)
		data[cur.index[id]] = merged
		return data, nil
	})
	if err != nil {
		return nil, nil, err
	}
	app.afterWrite(ctx, key, who.entry(key, id, "patch", before, merged))
	return merged, n, nil
}

// mergePatch applies RFC 7386 to a copy of row (nested objects merge too).
func mergePatch(row, patch map[string]any) map[string]any {
	out := maps.Clone(row)
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(out, k)
		case map[string]any:
			if cur, ok := out[k].(map[string]any); ok {
				out[k] = mergePatch(cur, pv)
			} else {
				out[k] = mergePatch(map[string]any{}, pv)
			}
		default:
			out[k] = v
		}
	}
	return out
}

func (app *App) deleteRecord(ctx context.Context, key, id, ifMatch string, who actor) error {
	var before map[string]any
	_, err := app.store.Mutate(key, func(cur *APISpec) ([]map[string]any, error) {
		old, exists := cur.record(id)
		if !exists {
			return nil, &httpErr{404, "record not found"}
		}
		if err := checkMatch(ifMatch, old, true); err != nil {
			return nil, err
		}
		before = old
		i := cur.index[id]
		return slices.Concat(cur.Data[:i], cur.Data[i+1:]), nil
	})
	if err != nil {
		return err
	}
	app.afterWrite(ctx, key, who.entry(key, id, "delete", before, nil))
	return nil
}

// ImportResult says what an import did.
type ImportResult struct {
	Created  int `json:"created"`
	Replaced int `json:"replaced"`
	Deleted  int `json:"deleted"`
	Total    int `json:"total"`
}

// importRecords loads many records at once. mode "upsert" creates or
// replaces by id; "replace" makes the API hold exactly these records.
// All rows are validated first: one bad row and nothing is written.
func (app *App) importRecords(ctx context.Context, key string, rows []map[string]any, mode string, who actor) (ImportResult, error) {
	var res ImportResult
	var es []AuditEntry
	_, err := app.store.Mutate(key, func(cur *APISpec) ([]map[string]any, error) {
		res, es = ImportResult{}, nil
		var data []map[string]any
		switch mode {
		case "upsert":
			data = slices.Clone(cur.Data)
		case "replace":
		default:
			return nil, &httpErr{400, `mode must be "upsert" or "replace"`}
		}
		pos := map[string]int{}
		for i, r := range data {
			pos[toString(r[cur.IDField])] = i
		}
		seen := map[string]bool{}
		for i, r := range rows {
			if r == nil {
				return nil, &httpErr{422, fmt.Sprintf("row %d: must be an object", i+1)}
			}
			if v, has := r[cur.IDField]; !has || v == nil || v == "" {
				return nil, &httpErr{422, fmt.Sprintf("row %d: %q is required for an import", i+1, cur.IDField)}
			}
			id := toString(r[cur.IDField])
			if err := validID(id); err != nil {
				return nil, &httpErr{422, fmt.Sprintf("row %d: %v", i+1, err)}
			}
			if seen[id] {
				return nil, &httpErr{422, fmt.Sprintf("row %d: %s %s appears twice", i+1, cur.IDField, id)}
			}
			seen[id] = true
			if err := cur.validate(r, false); err != nil {
				return nil, &httpErr{422, fmt.Sprintf("row %d (%s %s): %v", i+1, cur.IDField, id, err)}
			}
			old, exists := cur.record(id)
			switch {
			case !exists:
				res.Created++
				es = append(es, who.entry(key, id, "import", nil, r))
			case string(rawJSON(old)) != string(rawJSON(r)):
				res.Replaced++
				es = append(es, who.entry(key, id, "import", old, r))
			}
			if j, ok := pos[id]; ok {
				data[j] = r
			} else {
				pos[id] = len(data)
				data = append(data, r)
			}
		}
		if mode == "replace" {
			for _, r := range cur.Data {
				if id := toString(r[cur.IDField]); !seen[id] {
					res.Deleted++
					es = append(es, who.entry(key, id, "import", r, nil))
				}
			}
		}
		if cur.MaxRecords > 0 && len(data) > cur.MaxRecords {
			return nil, &httpErr{409, fmt.Sprintf("the import leaves %d records; this API holds at most %d (max_records)", len(data), cur.MaxRecords)}
		}
		res.Total = len(data)
		return data, nil
	})
	if err != nil {
		return res, err
	}
	app.afterWrite(ctx, key, es...)
	return res, nil
}

// revert undoes one audit entry: the record goes back to how it was before
// that change (re-created if it was deleted, removed if it was created).
// A contract change is reverted to the previous contract.
func (app *App) revert(ctx context.Context, e AuditEntry, who actor) (AuditEntry, error) {
	if app.store.Get(e.API) == nil {
		return AuditEntry{}, &httpErr{409, "api " + e.API + " no longer exists; deleted API files are kept in apis/.deleted/"}
	}
	switch {
	case e.Action == "api_update":
		var c APISpec
		if err := json.Unmarshal(e.Before, &c); err != nil {
			return AuditEntry{}, err
		}
		before, err := decodeSpec(e.Before)
		if err != nil {
			return AuditEntry{}, err
		}
		cur := app.store.Get(e.API)
		restoreSecrets(before, cur)
		old := maskedContract(cur)
		n, err := app.store.SaveContract(e.API, before)
		if err != nil {
			return AuditEntry{}, err
		}
		re := who.entry(n.key(), "", "revert", old, maskedContract(n))
		app.afterWrite(ctx, e.API, re)
		return re, nil
	case strings.HasPrefix(e.Action, "api_"):
		return AuditEntry{}, &httpErr{409, "creating or deleting a whole API cannot be reverted here; deleted API files are kept in apis/.deleted/"}
	case e.RecordID == "":
		return AuditEntry{}, &httpErr{409, "nothing to revert"}
	}
	var target map[string]any
	if e.Before != nil {
		dec := json.NewDecoder(strings.NewReader(string(e.Before)))
		dec.UseNumber()
		if err := dec.Decode(&target); err != nil {
			return AuditEntry{}, err
		}
	}
	var before map[string]any
	_, err := app.store.Mutate(e.API, func(cur *APISpec) ([]map[string]any, error) {
		old, exists := cur.record(e.RecordID)
		before = old
		data := slices.Clone(cur.Data)
		switch {
		case target == nil && !exists:
			return nil, &httpErr{409, "already reverted: the record does not exist"}
		case target == nil:
			i := cur.index[e.RecordID]
			return slices.Concat(data[:i], data[i+1:]), nil
		}
		if err := cur.validate(target, false); err != nil {
			return nil, &httpErr{422, "the earlier version is not valid under the current contract: " + err.Error()}
		}
		if exists {
			data[cur.index[e.RecordID]] = target
			return data, nil
		}
		return append(data, target), nil
	})
	if err != nil {
		return AuditEntry{}, err
	}
	re := who.entry(e.API, e.RecordID, "revert", before, target)
	app.afterWrite(ctx, e.API, re)
	return re, nil
}

// restoreSecrets puts the stored credentials back where a contract sent by
// the console carries Mask (it never sees literal tokens or passwords).
func restoreSecrets(in, cur *APISpec) {
	if cur == nil {
		return
	}
	for i, t := range in.Auth.Tokens {
		if t == Mask && i < len(cur.Auth.Tokens) {
			in.Auth.Tokens[i] = cur.Auth.Tokens[i]
		}
	}
	for u, p := range in.Auth.Users {
		if p == Mask {
			if old, ok := cur.Auth.Users[u]; ok {
				in.Auth.Users[u] = old
			}
		}
	}
}

// hasMask reports secrets that could not be restored (sent as Mask but new).
func hasMask(s *APISpec) error {
	for _, t := range s.Auth.Tokens {
		if t == Mask {
			return errors.New("auth.tokens: a new token cannot be " + Mask)
		}
	}
	for u, p := range s.Auth.Users {
		if p == Mask {
			return errors.New("auth.users." + u + ": a new password cannot be " + Mask)
		}
	}
	return nil
}
