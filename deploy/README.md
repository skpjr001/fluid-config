# Deploying fluid-config

fluid-config is one static binary with two listeners: the **APIs** (`api_addr`, default `:8090`) and the **console** (`admin_addr`, default `127.0.0.1:9090`). All state lives in two places:

- `dynamic_dir`: the API files with their records, `admin.json` and `access.json`. **Back it up**: records written through the API or the console live here.
- `database.path`: SQLite with the request logs and the change history.

At startup fluid-config logs **deployment notes** for the way it is running (container or host, listeners, TLS, allowlists, storage), at the level each deserves. They are also in `fluid-config check` and the console's **Status** page. Read the warnings once after every new setup.

## Installer for a Linux VM

```bash
curl -fsSL https://raw.githubusercontent.com/skpjr001/fluid-config/main/deploy/install.sh | sudo bash
```

The installer works on Ubuntu, Debian, Fedora, RHEL, Rocky, Alma, Amazon Linux, openSUSE and Alpine, on amd64 or arm64. It goes through these steps:

1. **Checks the system:** architecture, memory, free disk, package manager, systemd, and access to GitHub.
2. **Installs Docker** Engine and the Compose plugin from Docker's official repositories when they are missing, and starts Docker. Without systemd it starts `dockerd` directly. Go is optional and not needed.
3. **Downloads the release** binary, verifies its SHA-256 against `checksums.txt`, and builds a small distroless image from it. With `--source` it builds from the git repository instead.
4. **Asks about storage:** Docker volumes (default), or host directories that you can edit with any editor and back up with the host. Host directories default to `/fluid/config/dynamic` and `/fluid/config/data`. The installer creates them if they are missing and gives them to the container's user (uid 65532, mode 0750), including files already inside, on every run. `/fluid/config` itself stays root-owned (0755).
5. **Asks about each listener separately**, the APIs (8090) and the console (9090). **Neither is exposed by default.** A bare port binds `127.0.0.1`; use `0.0.0.0:PORT` for all interfaces.
6. **Asks for an optional console allowlist** and writes it to `access.json`.
7. **Writes files** to `/opt/fluid-config`: `docker-compose.yml`, `config.json` and a `.env` (0600) for the secrets that `env:` tokens in API files read. Then it starts the container and waits until it is healthy.

Re-running it upgrades in place, keeping the config, data and your earlier answers.

| Option | |
|---|---|
| `--version v0.2.0` | a specific release (default: latest) |
| `--source` | build from git instead of the release binary |
| `--storage volume\|bind`, `--data-path DIR` | where data lives (`bind` default: `/fluid/config`) |
| `--expose-api no\|PORT\|IP:PORT` | publish the APIs |
| `--expose-ui no\|PORT\|IP:PORT` | publish the console |
| `--allow-ui LIST` | console allowlist (IPs, CIDRs, `10.0.*.*` wildcards) |
| `--yes` | never prompt (nothing exposed unless `--expose-*`) |
| `--uninstall [--purge]` | remove the container (and with `--purge`, all data) |
| `--force` | continue with little free disk space |

Afterwards:

```bash
sudo docker exec fluid-config /fluid-config token -config /app/config.json    # console tokens
sudo docker exec fluid-config /fluid-config check -config /app/config.json    # validate + notes
sudo docker exec fluid-config /fluid-config logs -config /app/config.json -since 1h
```

> **Docker and allowlists.** With Docker's default userland proxy, clients that reach a published port can appear to come from the bridge gateway (`172.17.0.1`). That is a *private* address, so an API without `allowed_ips` (the restricted default) lets them in. If the APIs are published beyond localhost, give every API file explicit `allowed_ips` and auth. Better, put a reverse proxy in front and list it in `trusted_proxies`. Check `source_ip` in the request logs to see what fluid-config sees.

## systemd

```bash
sudo useradd -r -s /usr/sbin/nologin -d /var/lib/fluid-config fluidcfg
sudo install -m 755 fluid-config /usr/local/bin/
sudo cp deploy/systemd/fluid-config.service /etc/systemd/system/
sudo systemctl enable --now fluid-config
sudo -u fluidcfg fluid-config token -config /var/lib/fluid-config/config.json
```

The unit sets up the following:

- `StateDirectory` (`/var/lib/fluid-config`): fluid-config creates its config, `dynamic/` and `data/` there.
- `ExecStartPre=fluid-config init`: an invalid file stops the start with a clear message.
- Hardening: `ProtectSystem=strict`, no capabilities. Uncomment `CAP_NET_BIND_SERVICE` for built-in TLS on 443.
- `TimeoutStopSec=30s`, longer than `shutdown_timeout`, so in-flight writes finish on `systemctl stop`.
- Secrets for `env:` tokens: `EnvironmentFile=/etc/fluid-config/fluid-config.env` (start from [`example.env`](../example.env)).

## Docker and Compose

```bash
docker build -t fluid-config .
docker run -d --name fluid-config --stop-timeout 30 \
  -p 127.0.0.1:8090:8090 -p 127.0.0.1:9090:9090 \
  -v fluid-dynamic:/app/dynamic -v fluid-data:/app/data fluid-config
```

- With no `config.json` mounted, fluid-config writes one with container defaults (the console on `:9090` inside the container), plus `dynamic/` with an example API and a generated editor token.
- A **bind-mounted file must exist on the host first**, or Docker creates a directory in its place (fluid-config detects this and says so).
- A **bind-mounted directory must be writable by uid 65532** (`sudo chown -R 65532 dynamic data`), or API writes answer 409.
- `stop_grace_period: 30s` (or `docker stop -t 30`) must be longer than `shutdown_timeout`; Docker's default of 10s can cut a shutdown short.

[`compose.yml`](../compose.yml) runs it behind an existing Traefik network: the APIs get TLS from Traefik, and the console stays on `127.0.0.1:9090`.

## Behind a reverse proxy

List the proxy in `trusted_proxies`, so allowlists, rate limits and logs use the real client address (`X-Forwarded-For` is walked from the right and only believed from those addresses).

- **Traefik:** [`traefik/compose.yml`](traefik/compose.yml) serves the APIs on a subdomain and the console under `/config-admin` with `StripPrefix`. The console follows `X-Forwarded-Prefix` from trusted proxies, or set `admin_base_path`.
- **nginx:** [`nginx/fluid-config.conf`](nginx/fluid-config.conf) has the APIs at the root and the console under `/config-admin/`, with buffering off for live updates.
- **Caddy:** [`caddy/Caddyfile`](caddy/Caddyfile) serves the APIs on a subdomain and the console under `/config-admin` with `handle_path`. Caddy does not send `X-Forwarded-Prefix` by itself, so the example adds it with `header_up` (or set `admin_base_path`), and `flush_interval -1` keeps live updates flowing.
- **Base paths:** with `api_base_path: "/config"` the APIs answer at `/config/v1/...`, and also without the prefix, so the proxy may strip it or not. `Location` headers and the OpenAPI document use the prefix.

## End-to-end test

[`e2e/test.sh`](e2e/test.sh) starts fluid-config with Docker Compose and checks every layout above against real proxies (also in CI):

- **nginx, Traefik and Caddy** with the same routing as the examples: the APIs on a subdomain root and under `/config` (with and without prefix stripping), and the console, its API and live updates under `/config-admin`. Each proxy also gets a write with a token, the `Location` header, a cache hit and the change history.
- **The real client address:** a caller on a network that only the proxies join must be refused by an API with the restricted default, let in by an octet range, and logged with its own address, never the proxy's.
- **Built-in TLS** with certificate files (HTTP/2, TLS 1.3 on the console, a read-only `dynamic/` that answers 409), and **ACME** against [Pebble](https://github.com/letsencrypt/pebble) with TLS-ALPN-01 and HTTP-01.
- **A first start with nothing mounted:** the files it creates, the token never logged, the restricted default for public callers, and a restart that creates nothing.
- **The deployment notes** each setup logs.

```bash
./deploy/e2e/test.sh
DOCKER_HOST=unix:///run/user/$UID/podman/podman.sock ./deploy/e2e/test.sh   # with Podman
```

## Built-in TLS

```json
"api_tls": { "cert_file": "/etc/ssl/fullchain.pem", "key_file": "/etc/ssl/privkey.pem" },
"admin_tls": { "acme": { "domains": ["config.example.com"], "email": "ops@example.com", "accept_tos": true,
                         "http_challenge_addr": ":80" } }
```

Certificate files are reloaded when they change. ACME (Let's Encrypt by default; set `directory_url` for staging or a private CA) uses TLS-ALPN-01 on 443, or HTTP-01 when `http_challenge_addr` is set. The deployment notes warn about expiring certificates and unreachable challenge ports.

## Monitoring

- `/livez` and `/readyz` on both listeners. `/readyz` checks the database and fails during shutdown, so set `shutdown_delay` behind load balancers.
- `GET /metrics` on the console listener (needs a viewer or editor token):
  - counters: `fluid_api_requests_total{api,method,code}`, `fluid_record_changes_total{api}`, `fluid_access_denied_total`, `fluid_rate_limited_total`, `fluid_config_reloads_total`;
  - `fluid_cache_requests_total{api,result}`: reads of caching APIs, `hit` or `miss`;
  - histogram: `fluid_api_request_duration_seconds{api}` (1 ms to 5 s buckets);
  - gauges: `fluid_config_errors`, `fluid_apis`, `fluid_cache_bytes`.
- **Traces:** set `telemetry.otlp_endpoint` (for example `http://otel-collector:4318`), or `OTEL_EXPORTER_OTLP_ENDPOINT`, to send OpenTelemetry spans over OTLP/HTTP. Each API request gets a server span named after the API (`GET v1/holidays`) with its status, client, credential and cache result. An incoming `traceparent` header continues the caller's trace. Each request's log row records the trace id, shown in the console's log view. `telemetry.headers` adds headers for the collector (an API key, for example), and `sample_ratio` keeps that share of new traces. Queued spans are flushed on a graceful stop.
- Logs are JSON on stdout and stored in SQLite (`fluid-config logs`, or the console). Every API request is logged with its full request and response unless `capture_payloads` is off.

## Upgrading from the first fluid-config

| Before | Now |
|---|---|
| `addr` | `api_addr` (and the console on `admin_addr`) |
| `apis_dir: ./apis` | `dynamic_dir: ./dynamic`, files in `dynamic/apis/` (move them) |
| `base_path` | `api_base_path` |
| `trust_forwarded_for: true` | `trusted_proxies: ["<proxy CIDR>"]` |
| `catalog_token` / `FLUID_CATALOG_TOKEN` | any console token, or `public_catalog: true` |
| API without `cidrs`: open to everyone | **restricted to loopback and private networks**; add `"allowed_ips": ["*"]` (with auth) to keep it public |

API files themselves need no changes: `cidrs` and `rate_limit_per_min` still work. fluid-config refuses old static settings with a message naming the new one.
