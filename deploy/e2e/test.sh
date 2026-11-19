#!/usr/bin/env bash
# Runs fluid-config behind nginx, Traefik and Caddy and checks every layout:
# APIs on a subdomain root and under a path (with and without prefix
# stripping), the console + its API + live updates under a stripped path,
# writes, cache, history and the real client address through each proxy.
# Then built-in TLS, ACME against Pebble, and a first start with no files.
set -uo pipefail
cd "$(dirname "$0")"
COMPOSE=${COMPOSE:-docker compose}
VIEWER=e2e-viewer-token-0123456789
WRITER=e2e-api-token-0123456789
fail=0
check() { # name, expected, actual
  if [[ "$3" == *"$2"* ]]; then printf '  ok    %s\n' "$1"; else printf '  FAIL  %s\n        want %q, got %q\n' "$1" "$2" "${3:0:300}"; fail=1; fi
}
lacks() { # name, unexpected, actual
  if [[ "$3" != *"$2"* ]]; then printf '  ok    %s\n' "$1"; else printf '  FAIL  %s\n        did not want %q\n' "$1" "$2"; fail=1; fi
}

# Test certificates: a private CA + leaf for tls.test, and Pebble's CA for ACME.
rm -rf .certs && mkdir -p .certs
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e CA" \
  -keyout .certs/ca.key -out .certs/ca.pem 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=tls.test" \
  -keyout .certs/tls.key -out .certs/tls.csr 2>/dev/null
printf 'subjectAltName=DNS:tls.test,DNS:localhost,IP:127.0.0.1\n' > .certs/ext
openssl x509 -req -in .certs/tls.csr -CA .certs/ca.pem -CAkey .certs/ca.key -CAcreateserial -days 1 \
  -extfile .certs/ext -out .certs/tls.crt 2>/dev/null
chmod 644 .certs/*                                   # readable by the container's non-root user

$COMPOSE up -d --quiet-pull pebble >/dev/null 2>&1
$COMPOSE cp pebble:/test/certs/pebble.minica.pem .certs/pebble.minica.pem >/dev/null
chmod 644 .certs/pebble.minica.pem
$COMPOSE up -d --build --quiet-pull >/dev/null 2>&1 || { $COMPOSE up -d --build; exit 1; }
trap '[[ $fail != 0 ]] && $COMPOSE logs --tail 40 app app-fresh app-tls app-acme-alpn app-acme-http pebble; $COMPOSE down -v >/dev/null 2>&1; rm -rf .certs' EXIT
for i in $(seq 1 60); do curl -fs -H 'Host: config.test' http://127.0.0.1:28090/readyz >/dev/null && break; sleep 1; done

n=0
for proxy in nginx:28090 traefik:38090 caddy:48090; do
  IFS=: read -r name port <<<"$proxy"
  U=http://127.0.0.1:$port
  CFG=(-H 'Host: config.test')
  EX=(-H 'Host: example.test')
  echo "== $name"
  check "API on subdomain root" '"name":"first"' "$(curl -s "${CFG[@]}" "$U/v1/items")"
  check "API under /config" '"name":"first"' "$(curl -s "${EX[@]}" "$U/config/v1/items")"
  check "write without a token refused" '401' "$(curl -s -o /dev/null -w '%{http_code}' "${EX[@]}" -X POST "$U/config/v1/items" -d '{"name":"x"}')"
  n=$((n + 1))
  hdr=$(curl -s -D - -o /dev/null "${EX[@]}" -H "Authorization: Bearer $WRITER" -X POST "$U/config/v1/items" -d "{\"name\":\"via $name\"}")
  check "write through the proxy" '201' "$hdr"
  check "Location carries the base path" 'Location: /config/v1/items/' "$hdr"
  check "record readable on the subdomain" "\"via $name\"" "$(curl -s "${CFG[@]}" "$U/v1/items?q=$name")"
  curl -s -o /dev/null "${CFG[@]}" "$U/v1/items"
  check "second read served from cache" 'X-Cache: HIT' "$(curl -s -D - -o /dev/null "${CFG[@]}" "$U/v1/items")"
  check "console index carries base" 'src="/config-admin/app.js"' "$(curl -s "${EX[@]}" "$U/config-admin/")"
  check "console deep link" 'src="/config-admin/app.js"' "$(curl -s "${EX[@]}" "$U/config-admin/apis/v1/items")"
  check "console asset" 'fluid-config console' "$(curl -s "${EX[@]}" "$U/config-admin/app.js" | head -c 300)"
  check "console API under /config-admin" '"role":"viewer"' "$(curl -s "${EX[@]}" -H "Authorization: Bearer $VIEWER" "$U/config-admin/api/whoami")"
  check "history shows the write with its token name" '"actor":"e2e","via":"api"' \
    "$(curl -s "${EX[@]}" -H "Authorization: Bearer $VIEWER" "$U/config-admin/api/audit?api=v1/items&limit=1")"
  check "live stream under /config-admin" ': connected' "$(curl -s -N --max-time 2 "${EX[@]}" -H "Authorization: Bearer $VIEWER" "$U/config-admin/api/stream?topics=logs")"
done

echo "== real client address through each proxy (caller 172.34.0.50 on a separate network)"
for host in nginx traefik caddy; do
  c() { $COMPOSE exec -T client curl -s -o /dev/null -w '%{http_code}' -H 'Host: config.test' "http://$host/v1/$1"; }
  check "$host: API without allowed_ips refuses a public caller" '403' "$(c private)"
  check "$host: octet range 172.34.0.40-60 lets it in" '200' "$(c wild)"
  sleep 0.5
  got=$(curl -s -H 'Host: example.test' -H "Authorization: Bearer $VIEWER" 'http://127.0.0.1:28090/config-admin/api/logs?kind=api&route=v1/wild&limit=1')
  check "$host passes the real client address" '"source_ip":"172.34.0.50"' "$got"
done
check "private caller passes the restricted default" '200' "$($COMPOSE exec -T tester curl -s -o /dev/null -w '%{http_code}' http://app:8090/v1/private)"
check "refusal logged with the API and caller" '"api":"v1/private"' "$($COMPOSE logs --no-log-prefix app 2>/dev/null | grep 'source IP not allowed' | grep '172.34.0.50')"
check "all $n proxied writes are in the file" "$n" "$($COMPOSE exec -T tester curl -s 'http://app:8090/v1/items?q=via&limit=1' | grep -o '"total":[0-9]*' | cut -d: -f2)"

echo "== built-in TLS, no reverse proxy (certificate files, dynamic/ read-only)"
R=(--resolve tls.test:58090:127.0.0.1 --resolve tls.test:59090:127.0.0.1 --cacert .certs/ca.pem)
for i in $(seq 1 30); do curl -fs "${R[@]}" https://tls.test:58090/livez >/dev/null && break; sleep 1; done
check "API over HTTPS" '"name":"first"' "$(curl -s "${R[@]}" https://tls.test:58090/v1/items)"
check "plain HTTP to the TLS listener gets 400, nothing served" 'HTTPS server' "$(curl -s --max-time 3 http://127.0.0.1:58090/v1/items)"
check "console over HTTPS + HTTP/2" '2 200' "$(curl -s "${R[@]}" -o /dev/null -w '%{http_version} %{http_code}' -H "Authorization: Bearer $VIEWER" https://tls.test:59090/api/whoami)"
check "console enforces TLS 1.3" '000' "$(curl -s "${R[@]}" -o /dev/null -w '%{http_code}' --tls-max 1.2 https://tls.test:59090/)"
check "write to a read-only dynamic dir answers 409" '409' "$(curl -s "${R[@]}" -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $WRITER" -X POST https://tls.test:58090/v1/items -d '{"name":"x"}')"

echo "== ACME certificates from Pebble"
T="$COMPOSE exec -T tester curl"
for host in acme-alpn.test:443 acme-http.test:8443; do
  name=${host%%:*}
  out=""
  for i in $(seq 1 30); do out=$($T -sk -v --max-time 20 "https://$host/v1/items" 2>&1) && [[ "$out" == *'"first"'* ]] && break; sleep 2; done
  check "$name: certificate issued and served" 'Pebble Intermediate' "$out"
done
check "acme-http.test: HTTP redirects to HTTPS" 'Location: https://acme-http.test' "$($T -s -D - -o /dev/null http://acme-http.test/v1/items)"

echo "== first start without any files (app-fresh)"
F="$COMPOSE exec -T app-fresh /fluid-config"
for i in $(seq 1 15); do $F healthcheck -config /app/config.json >/dev/null 2>&1 && break; sleep 1; done
check "fresh: live on built-in defaults" '"live":true' "$($COMPOSE exec -T tester curl -s http://app-fresh:8090/livez)"
flogs=$($COMPOSE logs --no-log-prefix app-fresh 2>/dev/null | grep '"topic":"bootstrap"')
check "fresh: config.json created" 'created static config /app/config.json' "$flogs"
check "fresh: example API created" 'created example API /app/dynamic/apis/example.json' "$flogs"
check "fresh: access.json created" 'created access allowlists /app/dynamic/access.json' "$flogs"
check "fresh: admin.json + token location logged" 'generated editor token named \"admin\"' "$flogs"
check "fresh: token value never logged" 'not-logged' "$($COMPOSE logs app-fresh 2>/dev/null | grep -qE '[0-9a-f]{64}' && echo leaked || echo not-logged)"
check "fresh: console reachable inside the container network" '"role":"editor"' \
  "$($COMPOSE exec -T tester curl -s -H "Authorization: Bearer $($F token -config /app/config.json | awk '$1=="editor"{print $3; exit}')" http://app-fresh:9090/api/whoami)"
check "fresh: example API refuses a public caller" '403' "$($COMPOSE exec -T client curl -s -o /dev/null -w '%{http_code}' http://app-fresh:8090/v1/example)"
check "fresh: example API serves a private caller" 'first record' "$($COMPOSE exec -T tester curl -s http://app-fresh:8090/v1/example)"
sleep 1
check "fresh: refusal logged with the caller" '"source_ip":"172.34.0.50"' "$($COMPOSE logs --no-log-prefix app-fresh 2>/dev/null | grep '"status":403')"
check "fresh: restricted-default warning" 'api v1/example accepts only loopback and private-network callers' "$($COMPOSE logs --no-log-prefix app-fresh 2>/dev/null | grep '"topic":"access"')"
check "fresh: created files are valid" 'OK: all config files are valid' "$($F check -config /app/config.json 2>&1)"
check "fresh: init on a second run creates nothing" 'nothing to create' "$($F init -config /app/config.json 2>&1)"
nboot() { $COMPOSE logs --no-log-prefix app-fresh 2>/dev/null | grep -c '"topic":"bootstrap"'; }
nstart() { $COMPOSE logs --no-log-prefix app-fresh 2>/dev/null | grep -c 'fluid-config started'; }
before=$(nboot)
$COMPOSE restart app-fresh >/dev/null 2>&1
for i in $(seq 1 20); do [[ $(nstart) -ge 2 ]] && break; sleep 1; done
check "fresh: restarted" '2' "$(nstart)"
check "fresh: restart keeps files (no bootstrap again)" "same $before" "same $(nboot)"

echo "== deployment notes logged for each setup"
logs() { $COMPOSE logs --no-log-prefix "$1" 2>/dev/null | grep '"kind":"deploy"'; }
check "app: container detected" 'running in a' "$(logs app | grep -o 'running in a [a-z]* container')"
lacks "app: volumes count as storage (no false warning)" 'not on a mounted volume' "$(logs app)"
check "app: reverse proxy trust explained" 'expecting TLS to be terminated by a reverse proxy at 172.30.0.0/24' "$(logs app)"
check "app: API base path" 'APIs at /config/<version>/<name>' "$(logs app)"
check "app: console follows X-Forwarded-Prefix" 'console base path follows X-Forwarded-Prefix' "$(logs app)"
check "app: restricted API warned" '"level":"WARN","msg":"api v1/private accepts only loopback and private-network callers' "$(logs app)"
check "app: cache note" '1 API(s) cache responses in memory' "$(logs app)"
check "app-tls: HTTPS without proxy" 'APIs are served over HTTPS directly' "$(logs app-tls)"
check "app-tls: certificate details" 'certificate /certs/tls.crt for tls.test, localhost, 127.0.0.1' "$(logs app-tls)"
check "app-tls: read-only dynamic dir warned" '"level":"WARN","msg":"/app/dynamic/apis is read-only' "$(logs app-tls)"
check "app-acme-alpn: TLS-ALPN-01" 'TLS-ALPN-01 challenges on port 443' "$(logs app-acme-alpn)"
check "app-acme-http: HTTP-01" 'HTTP-01 challenges on :80' "$(logs app-acme-http)"

[[ $fail == 0 ]] && echo "ALL E2E CHECKS PASSED" || echo "E2E FAILURES"
exit $fail
