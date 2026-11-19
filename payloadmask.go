package main

import (
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// GET /api/logs/{id}: one log row with its captured request and response.
// Editors get everything exactly as stored. Viewers get the same with
// credentials masked: the values of credential headers, and any occurrence
// of a secret fluid-config knows (API tokens and passwords, console tokens).

const payloadMask = "********"

// alwaysSecretHeaders carry credentials in any API.
var alwaysSecretHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
	"X-Api-Key", "Api-Key", "X-Auth-Token", "X-Access-Token", "X-Csrf-Token"}

// secretsForMasking collects header names and values fluid-config knows to
// be credentials. Values shorter than 6 characters are skipped (too common).
func (a *Admin) secretsForMasking() (headers map[string]bool, values []string) {
	headers = map[string]bool{}
	for _, h := range alwaysSecretHeaders {
		headers[http.CanonicalHeaderKey(h)] = true
	}
	add := func(v string) {
		if len(v) >= 6 {
			values = append(values, v)
		}
	}
	for _, t := range a.tokens() {
		add(t.Token)
	}
	for _, s := range a.app.store.All() {
		if s.Auth.Header != "" {
			headers[http.CanonicalHeaderKey(s.Auth.Header)] = true
		}
		for _, v := range s.Auth.plainSecrets() {
			add(v)
		}
	}
	// Longest first, so a secret containing another is replaced whole.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return headers, values
}

func maskString(s string, values []string) string {
	for _, v := range values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, payloadMask)
		}
	}
	return s
}

func maskPart(p *PayloadPart, headers map[string]bool, values []string) {
	if p == nil {
		return
	}
	for k, vs := range p.Headers {
		for i, v := range vs {
			if headers[http.CanonicalHeaderKey(k)] {
				vs[i] = payloadMask
			} else {
				vs[i] = maskString(v, values)
			}
		}
	}
	p.Body = maskString(p.Body, values)
}

func (a *Admin) getLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		errJSON(w, 400, "bad id")
		return
	}
	rows, _, err := QueryLogs(r.Context(), a.app.db, LogQuery{List: ListParams{Conds: []string{"id = ?"}, Args: []any{id}}, Limit: 1})
	if err != nil {
		errJSON(w, 500, err.Error())
		return
	}
	if len(rows) == 0 {
		errJSON(w, 404, "not found")
		return
	}
	p, err := GetLogPayload(r.Context(), a.app.db, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		errJSON(w, 500, err.Error())
		return
	}
	masked := false
	if principalOf(r).Role != RoleEditor && p != nil {
		hs, vs := a.secretsForMasking()
		maskPart(p.Request, hs, vs)
		maskPart(p.Response, hs, vs)
		masked = true
	}
	writeJSON(w, 200, map[string]any{"log": rows[0], "payload": p, "masked": masked})
}
