package main

import (
	"net/http"
	"sort"
)

// openAPI builds an OpenAPI 3.0 document from every enabled spec, so the
// generated APIs show up in Swagger UI / Postman / client generators.
func (s *Server) openAPI(r *http.Request) map[string]any {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	paths := map[string]any{}
	schemas := map[string]any{}
	secSchemes := map[string]any{}
	errResp := map[string]any{"description": "error", "content": jsonContent(ref("Error"))}
	schemas["Error"] = map[string]any{"type": "object", "properties": map[string]any{
		"error": map[string]any{"type": "object", "properties": map[string]any{
			"code": map[string]any{"type": "integer"}, "message": map[string]any{"type": "string"}}}}}

	for _, a := range s.store.All() {
		if !a.enabled() {
			continue
		}
		model := a.Version + "_" + a.Name
		schemas[model] = recordSchema(a)

		var security []any
		if a.Auth.Type != "none" {
			name := "auth_" + model
			secSchemes[name] = securityScheme(a.Auth)
			security = []any{map[string]any{name: []any{}}}
		}
		op := func(method, summary string, params []any, body bool, okCode string, okSchema any) map[string]any {
			o := map[string]any{"summary": summary, "tags": []any{a.Version + "/" + a.Name}}
			if len(params) > 0 {
				o["parameters"] = params
			}
			if body {
				o["requestBody"] = map[string]any{"required": true, "content": jsonContent(ref(model))}
			}
			resp := map[string]any{"default": errResp}
			if okSchema != nil {
				resp[okCode] = map[string]any{"description": "ok", "content": jsonContent(okSchema)}
			} else {
				resp[okCode] = map[string]any{"description": "ok"}
			}
			o["responses"] = resp
			if security != nil && !(a.Auth.ReadPublic && method == "GET") {
				o["security"] = security
			}
			return o
		}
		one := map[string]any{"type": "object", "properties": map[string]any{"data": ref(model)}}
		many := map[string]any{"type": "object", "properties": map[string]any{
			"data": map[string]any{"type": "array", "items": ref(model)},
			"meta": map[string]any{"type": "object"}}}
		listParams := []any{
			qp("q", "full-text search across fields"), qp("sort", "e.g. date,-amount"),
			qp("page", "1-based page"), qp("limit", "page size"), qp("fields", "comma-separated projection"),
		}
		for _, name := range sortedKeys(a.Fields) {
			listParams = append(listParams, qp(name, "filter; also "+name+"[gt|gte|lt|lte|ne|like|in|exists]"))
		}
		idParam := []any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}}

		col, item := map[string]any{}, map[string]any{}
		if a.allows("GET") {
			col["get"] = op("GET", "List "+a.Name, listParams, false, "200", many)
			item["get"] = op("GET", "Get one "+a.Name, idParam, false, "200", one)
		}
		if a.allows("POST") {
			col["post"] = op("POST", "Create "+a.Name, nil, true, "201", one)
		}
		if a.allows("PUT") {
			item["put"] = op("PUT", "Replace / upsert "+a.Name, idParam, true, "200", one)
		}
		if a.allows("PATCH") {
			item["patch"] = op("PATCH", "Merge-patch "+a.Name+" (null removes a field)", idParam, true, "200", one)
		}
		if a.allows("DELETE") {
			item["delete"] = op("DELETE", "Delete "+a.Name, idParam, false, "204", nil)
		}
		base := s.cfg.BasePath + a.basePath()
		if len(col) > 0 {
			col["description"] = a.Description
			paths[base] = col
		}
		if len(item) > 0 {
			paths[base+"/{id}"] = item
		}
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info":    map[string]any{"title": "fluid-config", "version": "1"},
		"servers": []any{map[string]any{"url": scheme + "://" + r.Host}},
		"paths":   paths,
		"components": map[string]any{
			"schemas": schemas, "securitySchemes": secSchemes,
		},
	}
}

func ref(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }

func jsonContent(schema any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schema}}
}

func qp(name, desc string) map[string]any {
	return map[string]any{"name": name, "in": "query", "required": false, "description": desc,
		"schema": map[string]any{"type": "string"}}
}

func sortedKeys(m map[string]*FieldSpec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func recordSchema(a *APISpec) map[string]any {
	props := map[string]any{}
	var required []any
	for _, name := range sortedKeys(a.Fields) {
		f := a.Fields[name]
		p := map[string]any{}
		if f.Type != "any" {
			p["type"] = f.Type
		}
		if f.Description != "" {
			p["description"] = f.Description
		}
		if len(f.Enum) > 0 {
			p["enum"] = f.Enum
		}
		if f.Pattern != "" {
			p["pattern"] = f.Pattern
		}
		if f.Format != "" {
			p["format"] = f.Format
		}
		if f.Type == "string" {
			if f.Min != nil {
				p["minLength"] = int(*f.Min)
			}
			if f.Max != nil {
				p["maxLength"] = int(*f.Max)
			}
		} else {
			if f.Min != nil {
				p["minimum"] = *f.Min
			}
			if f.Max != nil {
				p["maximum"] = *f.Max
			}
		}
		props[name] = p
		if f.Required {
			required = append(required, name)
		}
	}
	if _, ok := props[a.IDField]; !ok {
		props[a.IDField] = map[string]any{"description": "record id (auto-generated if omitted on create)"}
	}
	sch := map[string]any{"type": "object", "properties": props, "additionalProperties": !a.StrictFields}
	if len(required) > 0 {
		sch["required"] = required
	}
	if a.Description != "" {
		sch["description"] = a.Description
	}
	return sch
}

func securityScheme(a AuthSpec) map[string]any {
	switch a.Type {
	case "bearer":
		return map[string]any{"type": "http", "scheme": "bearer"}
	case "basic":
		return map[string]any{"type": "http", "scheme": "basic"}
	default: // api_key
		return map[string]any{"type": "apiKey", "in": "header", "name": a.Header}
	}
}
