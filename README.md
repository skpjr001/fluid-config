# fluid-config

Drop a JSON file in `apis/` and you get a REST API. One file = one API: its contract, its security and its data.
Single Go binary, zero dependencies, hot-reloaded like Traefik's file provider.

```
apis/
  ipo-config.json          -> /v1/ipo-config
  market/holidays.json     -> /v1/holidays        (sub-folders are just for grouping)
```

## Run

```bash
go build -o fluid-config .
cp config.example.json config.json
./fluid-config -check      # validate every file, non-zero exit on errors (CI / pre-commit)
./fluid-config             # serve on :8090
```
Docker/Traefik: `cp config.docker.json config.json`, put secrets in `.env`, set the host in `compose.yml`, `docker compose up -d` (podman-compose works too).

## API file

```jsonc
{
  "name": "holidays",                 // [a-z0-9_-], URL segment
  "version": "v1",                    // v1, v2, v1-beta ... -> /v1/holidays
  "description": "NSE/BSE trading holidays",
  "enabled": true,                    // false -> 404, file kept
  "id_field": "id",                   // default "id"; e.g. "symbol"
  "methods": ["GET","POST","PUT","PATCH","DELETE"],   // default ["GET"] = read-only
  "auth": {
    "type": "bearer",                 // none | bearer | api_key | basic
    "tokens": ["env:HOLIDAYS_TOKEN"], // literal, "env:VAR" or "sha256:<hex>"
    "header": "X-API-Key",            // api_key only
    "users": {"ops": "env:OPS_PASS"}, // basic only
    "read_public": true               // GETs need no auth, writes do
  },
  "cidrs": ["10.0.0.0/8", "203.0.113.7"],
  "rate_limit_per_min": 600,          // per client IP, 0 = off
  "cache_seconds": 300,               // Cache-Control max-age (ETag/304 always on)
  "cors_origins": ["https://www.arihantcapital.com"],
  "strict_fields": true,              // reject fields not declared below
  "fields": {
    "date":     {"type": "string", "format": "date", "required": true},
    "exchange": {"type": "string", "enum": ["NSE","BSE","MCX"], "required": true},
    "reason":   {"type": "string", "min": 2, "max": 120}
  },
  "data": [ { "id": 1, "date": "2026-01-26", "exchange": "NSE", "reason": "Republic Day" } ]
}
```
Field types: `string number integer boolean object array any`. Rules: `required enum min max pattern format(date|date-time|email)`. `min`/`max` apply to the value for numbers and to the length for strings.
Make a hash: `printf '%s' 'the-token' | sha256sum` and use `"sha256:<hash>"`, so the file never holds the real token.

## Generated endpoints

| Method | Path | Notes |
|---|---|---|
| GET | `/v1/holidays` | list: filters, `q`, `sort`, `page`, `limit`, `fields` |
| GET | `/v1/holidays/{id}` | one record, `?fields=` works |
| POST | `/v1/holidays` | create; id auto-generated if omitted (max+1 for numeric ids, else random hex) |
| PUT | `/v1/holidays/{id}` | replace, or create if missing (201) |
| PATCH | `/v1/holidays/{id}` | JSON Merge Patch; `null` removes a field |
| DELETE | `/v1/holidays/{id}` | 204 |
| GET | `/v1/holidays/_schema` | the contract, no data, no secrets |
| GET | `/_catalog` | every API (protect with `catalog_token`) |
| GET | `/openapi.json` | OpenAPI 3 for all APIs: load into Swagger UI / Postman |

Only methods listed in `methods` exist (others get 405).

### Querying
```
?exchange=NSE                         equals
?date[gte]=2026-03-01&date[lt]=2026-12-31
?exchange[in]=BSE,MCX   ?reason[like]=day   ?segment[exists]=false   ?exchange[ne]=MCX
?price_band.high[gte]=200             nested paths with dots
?q=diwali                             search across all fields
?sort=-date,exchange                  "-" = descending
?page=2&limit=50&fields=date,reason
```
Values compare as numbers when both sides are numeric, otherwise as strings, so ISO dates sort correctly.
Response: `{"data": [...], "meta": {"total", "page", "limit", "pages", "api", "version"}}`. Errors: `{"error": {"code", "message"}}`.

## How it behaves
- **Hot reload.** Files are polled every `reload_interval`. Add, edit or delete a file and the API changes, no restart needed.
- **Broken file.** It's logged, and the last good version keeps serving. Two files claiming the same `version/name`: the second is skipped.
- **Writes go back to the same file.** It's rewritten atomically (temp file + rename) and serialized, so concurrent writes never lose records. Numbers keep full precision. `env:`/`sha256:` secrets stay as written.
- **Reads are lock-free** against an in-memory snapshot.
- **Size.** Each API is held in memory and its file is rewritten on every write. That's right for config and reference data (hundreds to tens of thousands of rows, occasional writes). It is not a database: high write rates or large datasets belong in Postgres/Mongo.
- **Write formatting.** A write re-serializes the file in a fixed field order, so comments and hand formatting in that file are lost. Keep docs in `description`.
- `trust_forwarded_for: true` only when it's behind Traefik/Nginx; otherwise `cidrs` and rate limits use the socket IP.
