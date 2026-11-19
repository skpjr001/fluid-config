# fluid-config

Drop a JSON file in `apis/` and you get a REST API: one file is one API, holding its contract, who may call it, and its data. It ships as a single Go binary with a web console, a change history and request logs.

```
dynamic/apis/
  ipo-config.json          -> /v1/ipo-config
  market/holidays.json     -> /v1/holidays        (sub-folders are only for grouping)
```

It runs two listeners:

| Listener | Default | Purpose |
|---|---|---|
| API | `:8090` | The generated REST APIs: list with filters, search, sort and paging; get; create; replace; merge-patch; delete. Per-API auth (bearer, API key, basic), **IP allowlists with wildcards**, rate limits, CORS, ETags, `If-Match`, and an optional **in-memory response cache**. Also `/_catalog` and `/openapi.json`. |
| Console | `127.0.0.1:9090` | Web console and admin API: create APIs and edit their contracts, browse/edit/import records, per-record **history with revert**, request logs with full request/response capture, cache stats and **purge**, access tester, deployment notes, Prometheus `/metrics`, OpenTelemetry traces. **Viewer** and **editor** tokens. |

API files are hot-reloaded and validated against published [JSON Schemas](schema/). Writes through the API or the console go back into the same file, atomically. Request logs and the change history live in one SQLite file.

## Quick start

No setup is needed: on first start fluid-config creates every directory and config file it is missing, including an example API (see [First start](#first-start-what-is-created)).

**On a fresh Linux VM** (Ubuntu, Debian, Fedora, RHEL/Rocky/Alma, Amazon Linux), the installer checks the system, installs Docker if needed and runs fluid-config in a container:

```bash
curl -fsSL https://raw.githubusercontent.com/skpjr001/fluid-config/main/deploy/install.sh | sudo bash
```

It asks whether to expose the APIs and the console on the host, each separately and **not exposed by default**. It also asks where to keep data (Docker volumes by default, or host directories) and which addresses may use the console. See [deploy/README.md](deploy/README.md#installer-for-a-linux-vm).

**On a host**

```bash
fluid-config                     # creates ./config.json, ./dynamic/ and ./data/, then serves
fluid-config token               # the generated editor token for the console
curl localhost:8090/v1/example   # the example API
```

**With systemd** (unit in [`deploy/systemd`](deploy/systemd/fluid-config.service))

```bash
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/fluid-config fluidcfg
sudo install -m 755 fluid-config /usr/local/bin/
sudo cp deploy/systemd/fluid-config.service /etc/systemd/system/
sudo systemctl enable --now fluid-config
sudo -u fluidcfg fluid-config token -config /var/lib/fluid-config/config.json
```

**In a container**

```bash
docker build -t fluid-config .
docker run -d --name fluid-config -p 127.0.0.1:8090:8090 -p 127.0.0.1:9090:9090 \
  -v fluid-dynamic:/app/dynamic -v fluid-data:/app/data fluid-config
docker exec fluid-config /fluid-config token -config /app/config.json
```

Keep `/app/dynamic` on a volume: records written through the API live in the API files there. For the Traefik setup in [`compose.yml`](compose.yml): `cp config.docker.json config.json`, `cp example.env .env` and fill in the secrets for `env:` tokens, set the host in `compose.yml`, then `docker compose up -d`.

**Then** open the console (through an SSH tunnel: `ssh -L 9090:127.0.0.1:9090 server`, then <http://localhost:9090>) and sign in with the token. Create an API with **APIs → Create API**, or drop a JSON file in `dynamic/apis/` and it is served within `reload_interval` (2s).

### First start: what is created

Existing files are never overwritten. Each item is logged (`kind: "deploy"`, `topic: "bootstrap"`) with its absolute path. Anything that cannot be created is logged as a warning, and fluid-config runs on built-in defaults.

| Path (defaults) | Contents |
|---|---|
| `config.json` (`-config`) | every static setting with its default value and `$schema` |
| `dynamic/apis/example.json` | a read-only example API, only when `apis/` is empty (`-no-example` skips it) |
| `dynamic/admin.json` (0600) | one generated **editor** token named `admin`, or none when `FLUID_ADMIN_TOKEN` is set |
| `dynamic/access.json` | console and catalog allowlists: loopback and private networks |
| `data/` | the SQLite database (and `acme/` when TLS uses ACME) |

`fluid-config init` does the same without serving and then validates everything; `fluid-config check` validates without creating anything (use it in CI and pre-commit).

## Commands

```
fluid-config [serve] [-config config.json]   run (default)
fluid-config init    [-config config.json]   create missing directories and files, then validate
fluid-config check   [-config config.json]   validate config.json, admin.json, access.json and every API file
fluid-config token   [-config config.json]   print the console tokens from admin.json
fluid-config logs    [flags]                 query the request log (read-only; works while running)
fluid-config healthcheck                     exit 0 if the local server is live (Docker HEALTHCHECK)
fluid-config version
```

## API file

```jsonc
{
  "$schema": "https://raw.githubusercontent.com/skpjr001/fluid-config/main/schema/api.schema.json",
  "name": "holidays",                 // [a-z0-9_-], URL segment
  "version": "v1",                    // v1, v2, v1-beta ... -> /v1/holidays
  "description": "NSE/BSE trading holidays",
  "enabled": true,                    // false -> 404, file kept
  "id_field": "id",                   // default "id"; e.g. "symbol"
  "methods": ["GET","POST","PUT","PATCH","DELETE"],   // default ["GET"] = read-only
  "auth": {
    "type": "bearer",                 // none | bearer | api_key | basic
    "tokens": ["ops=env:HOLIDAYS_TOKEN", "ci=sha256:<hex>"],
    "header": "X-API-Key",            // api_key only
    "users": {"ops": "env:OPS_PASS"}, // basic only
    "read_public": true               // GETs need no credentials, writes do
  },
  "allowed_ips": ["52.66.*.*", "10.0.0.0/8", "203.0.113.7"],  // omitted = loopback + private networks only
  "rate_limit": {"requests_per_minute": 600, "burst": 100},    // per client IP
  "cache_seconds": 300,               // Cache-Control max-age for clients (ETag/304 always on)
  "cache": {"enabled": true, "ttl_seconds": 300, "max_entries": 500},   // in-memory cache on the server
  "cors_origins": ["https://www.example.com"],
  "strict_fields": true,              // refuse fields not declared below
  "max_records": 10000,               // refuse creates beyond this
  "fields": {
    "date":     {"type": "string", "format": "date", "required": true},
    "exchange": {"type": "string", "enum": ["NSE","BSE","MCX"], "required": true},
    "reason":   {"type": "string", "min": 2, "max": 120}
  },
  "data": [ { "id": 1, "date": "2026-01-26", "exchange": "NSE", "reason": "Republic Day" } ]
}
```

- **Field types:** `string number integer boolean object array any`. Rules: `required enum min max pattern format` (`date`, `date-time`, `email`, `uri`). `min`/`max` limit the value for numbers and the length for strings.
- **Secrets** are a literal, `env:VAR` (read from the environment) or `sha256:<hex>` (only the hash is stored). Prefix an `env:` or `sha256:` token with `name=` to name it in logs and the history. Literal tokens are named `token-1`, `token-2`… by position.
- **Hashing a token:** `printf '%s' 'the-token' | sha256sum`. The console's **Generate token** button does this for you: it shows the token once and stores only the hash.
- **Compatibility:** files from the first release keep working. `cidrs` is combined with `allowed_ips`, and `rate_limit_per_min` still works (burst = the same number).

## Generated endpoints

| Method | Path | Notes |
|---|---|---|
| GET | `/v1/holidays` | list: filters, `q`, `sort`, `page`, `limit`, `fields` |
| GET | `/v1/holidays/{id}` | one record; its `ETag` is the record's version |
| POST | `/v1/holidays` | create; id generated if omitted (max+1 for numeric ids, else random hex) |
| PUT | `/v1/holidays/{id}` | replace, or create if missing (201) |
| PATCH | `/v1/holidays/{id}` | JSON Merge Patch (RFC 7386); `null` removes a field |
| DELETE | `/v1/holidays/{id}` | 204 |
| GET | `/v1/holidays/_schema` | the contract, no data, no secrets |
| GET | `/_catalog`, `/openapi.json` | every API, and OpenAPI 3 for Swagger UI / Postman (need a console token unless `public_catalog`) |
| GET | `/livez`, `/readyz`, `/healthz` | health (`/readyz` fails while shutting down) |

Only the methods listed in `methods` exist; others get 405. Writes accept **`If-Match: "<etag>"`**: if someone changed the record since you read it, the write fails with 412 instead of overwriting it. Every response carries `X-Request-Id`, which also appears in the logs and the history.

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

## Who can call an API

Access is checked in this order, before credentials:

1. **`allowed_ips`** (plus `cidrs`). Each entry is `*`, an IP, a CIDR, or an IPv4 wildcard such as `52.66.*.*`, `10.*` or `192.168.1-5.*`. **An API without a list is restricted by default** to loopback and private networks (`127.0.0.0/8, ::1, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7`), so nothing is reachable from the internet until its file says so. Other callers get 403, and the refusal is logged with their address.
2. The **rate limit** (429 with `Retry-After`).
3. **Auth.** With `read_public`, GETs skip it.

Startup and every change log a warning for each API left on the restricted default, and an error for an API that accepts writes from anyone without auth. The **Access** page lists every API's callers and has a tester ("would 52.66.10.20 be let in?").

`access.json` holds two listener-wide lists:

- `admin`: who may use the console. It can only be changed in the file, so nobody can lock the console or open it from the console itself.
- `catalog`: who may read `/_catalog` and `/openapi.json`.

Behind a reverse proxy, list it in `trusted_proxies` so the real client address (from `X-Forwarded-For`, walked from the right) is checked.

## In-memory cache

For read-heavy config data, set `"cache": {"enabled": true}` in an API file:

- GET responses are kept in memory per query, until `ttl_seconds` (default 60) pass or **the data changes**. Any write, import, revert or hand edit of the file makes older answers unreachable at once.
- At most `max_entries` (default 500) distinct queries are kept, least recently used dropped first.
- Answers come from the cache only after the IP allowlist, rate limit and auth have passed.
- Responses say `X-Cache: HIT` or `MISS`, and `Cache-Control: no-cache` from a client skips the cache.
- The console's **Cache** tab shows entries, memory and hit rate, with a per-API **Purge cache** button.

`cache_seconds` is separate: it tells browsers and CDNs how long they may keep answers.

## Console

Sign in with a token from `admin.json` (`fluid-config token`), or with `FLUID_ADMIN_TOKEN`:

- **Viewers** see everything, with secrets masked.
- **Editors** can also create and change APIs and records, import, revert and purge caches.

What it offers:

- **APIs**: every file with its status, record count, auth, allowed IPs, cache and recent changes. An invalid file is flagged, and its previous version keeps serving.
- **Create API**: a form for the whole contract, with a field builder, methods, auth with **Generate token**, allowed IPs, rate limit, CORS and cache. You can start blank, from an example, or paste a full API file. **Validate** checks it before anything is written, and the new file appears at once (sub-folders allowed: `market/holidays.json`).
- **Records**: a server-side table with a filter per column (contains, `=exact`, `!not`, `>=x`, `a..b`), multi-column sort, paging and CSV export of the filtered rows. Add, edit and delete records with a form generated from the contract (or raw JSON). Import a JSON array (upsert or replace-all, validated as a whole), or download all records as JSON.
- **Contract**: behaviour, access and fields at a glance. **Edit contract** changes everything but the data, and refuses a contract the existing records would not satisfy. Literal tokens never reach the browser.
- **History**: every change with who, when and how (API credential, console user, or `file` for a hand edit), the record before and after side by side, and **Revert**. There is a global History page too.
- **Requests**: each call to the API with its full request and response (RQH/RQB/RSH/RSB). Editors see them raw; viewers see credentials masked.
- **Cache**, **Use it** (copy-paste curl for every method, plus an OpenAPI download), **Access** and **Status** (deployment notes).

Deleting an API from the console moves its file to `apis/.deleted/`, so you can restore it by moving it back.

## How it behaves

- **Hot reload.** `dynamic_dir` is polled every `reload_interval`. Add, edit or delete a file and the API changes with no restart.
- **Hand edits are history too.** When a file changes on disk, the differences are recorded per record (`via: file`) and the API's cache is dropped.
- **A broken file** is reported (log, `check`, console), and its last good version keeps serving. Writes to it are refused until it is fixed, so they cannot discard the hand edit. When two files claim the same `version/name`, the second is ignored.
- **Writes never overwrite a hand edit.** Before writing, fluid-config re-reads the file if it changed on disk. The file is then replaced atomically (temp file and rename), and writes are serialized, so concurrent writes never lose records. Numbers keep full precision, and `env:` and `sha256:` secrets stay as written.
- **Reads are lock-free** against an in-memory snapshot.
- **Size.** Each API is held in memory and its file is rewritten on every write. That suits config and reference data: hundreds to tens of thousands of rows, with occasional writes. High write rates or large datasets belong in a database.
- **Write formatting.** A write re-serializes the file in a fixed field order, so comments and hand formatting in that file are lost. Keep docs in `description`.
- **Read-only files** (a `:ro` mount) answer writes with 409 and a clear message, and a deployment note says so.
- **Graceful shutdown.** On SIGTERM, `/readyz` fails. With `shutdown_delay`, fluid-config keeps serving that long, then gives in-flight requests up to `shutdown_timeout` (15s) to finish, flushes logs and closes the database. A second signal stops at once. Give containers a longer stop timeout: `stop_grace_period: 30s`.

## Static config

`config.json` is read once at startup; see [`config.example.json`](config.example.json) and the [schema](schema/config.schema.json).

| Setting | Default | |
|---|---|---|
| `api_addr`, `admin_addr` | `:8090`, `127.0.0.1:9090` | listeners (in a container, a generated config uses `:9090` for the console) |
| `api_base_path`, `admin_base_path` | `""` | path prefixes; requests work with or without them |
| `api_tls`, `admin_tls` | off | HTTPS from certificate files (reloaded on change) or ACME (Let's Encrypt) |
| `dynamic_dir` | `./dynamic` | `apis/`, `admin.json`, `access.json` |
| `trusted_proxies` | `[]` | reverse proxies whose `X-Forwarded-For` is believed |
| `public_catalog` | `false` | serve `/_catalog` and `/openapi.json` without a token |
| `max_body_bytes`, `max_page_size` | 1 MiB, 1000 | |
| `shutdown_timeout`, `shutdown_delay` | `15s`, `0s` | graceful shutdown |
| `database.retention_days` | 14 | request logs |
| `database.audit_retention_days` | 365 | change history |
| `database.capture_payloads`, `capture_max_body_bytes` | `true`, 1 MiB | full request/response capture |
| `telemetry.otlp_endpoint` | `""` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) | export OpenTelemetry traces over OTLP/HTTP; empty is off |
| `telemetry.service_name`, `sample_ratio`, `headers` | `fluid-config`, 1, none | trace resource name, share of new traces kept, headers for the collector |

Settings from the first release (`addr`, `apis_dir`, `base_path`, `trust_forwarded_for`, `catalog_token`) are refused with a message naming their replacement.

## Deployment

See [deploy/README.md](deploy/README.md) for the following:

- the VM installer;
- systemd;
- Docker and Compose;
- Traefik, nginx and Caddy in front (TLS, console under a path, real client addresses);
- built-in TLS and ACME;
- the deployment notes fluid-config prints for each setup.

Every one of these layouts is covered by an end-to-end test behind the real proxies (also run in CI).

## Development

```bash
go test -race ./...         # unit and integration tests
./deploy/e2e/test.sh        # behind real nginx, Traefik and Caddy, plus TLS and ACME (Docker or Podman)
go vet ./... && gofmt -l .
node --check ui/app.js
```

The console is plain JavaScript embedded in the binary, with no build step. Its strict CSP allows no inline scripts or styles, and all DOM is built with `textContent`. The schemas in [`schema/`](schema) are checked against the Go structs by the tests.

## License

[MIT](LICENSE)
