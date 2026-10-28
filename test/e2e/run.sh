#!/usr/bin/env bash
# Runs the hub and a connector as containers on separated networks (see
# docker-compose.yml) and checks that a client can reach Postgres in the
# customer network only through the hub, and only on an allowlisted port.
set -euo pipefail
cd "$(dirname "$0")"

export E2E_UID E2E_GID E2E_PKI
E2E_UID=$(id -u)
E2E_GID=$(id -g)
E2E_PKI=$(mktemp -d "${TMPDIR:-/tmp}/edge-e2e.XXXXXX")

compose() { docker compose "$@"; }
cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    compose logs hub connector || true
  fi
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$E2E_PKI"
  exit "$status"
}
trap cleanup EXIT

pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; exit 1; }
client() { compose run --rm --no-deps -T client "$@"; }

compose build --quiet
edgectl() {
  compose run --rm --no-deps -T --user "$E2E_UID:$E2E_GID" -v "$E2E_PKI:/out" \
    --entrypoint /usr/local/bin/edgectl hub "$@" >/dev/null
}
edgectl ca -name e2e -out /out
edgectl issue -ca /out -name hub -dns hub.test -server -out /out
edgectl issue -ca /out -name connector-1 -client -out /out

compose up --detach --wait postgres hub connector

out=$(client psql -h hub -tAc "select 'through the tunnel'")
[ "$out" = "through the tunnel" ] || fail "query through the hub returned: $out"
pass "client queries Postgres through the hub"

bytes=$(client sh -c "psql -h hub -c 'copy (select generate_series(1, 2000000)) to stdout' | wc -c")
[ "$bytes" -eq 14888896 ] || fail "bulk copy returned $bytes bytes"
pass "bulk copy of $bytes bytes arrives intact"

if client psql -h hub -p 5433 -tAc 'select 1' >/dev/null 2>&1; then
  fail "a target off the allowlist was reachable"
fi
compose logs connector | grep -q '"reason":"denied"' || fail "connector did not log the denial"
pass "target off the allowlist is refused, and the connector logs it"

if client pg_isready -h postgres -t 3 >/dev/null 2>&1; then
  fail "client reached Postgres directly"
fi
pass "client cannot reach the customer network directly"
