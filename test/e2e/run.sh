#!/usr/bin/env bash
# Runs the hub and a connector as containers on separated networks (see
# docker-compose.yml). The connector enrolls with a token. Checks that a
# client can reach Postgres in the customer network only through the hub, and
# only on an allowlisted port, and that the connector comes back from its
# state volume without the token.
set -euo pipefail
cd "$(dirname "$0")" || exit

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
# Whether the connector logged a line matching $1. grep reads the whole log:
# "grep -q" stops at the first match, and under pipefail the SIGPIPE that
# kills "compose logs" would fail the check.
connector_logged() { compose logs connector | grep -- "$1" >/dev/null; }
fail() { printf 'FAIL  %s\n' "$*"; exit 1; }
client() { compose run --rm --no-deps -T client "$@"; }

compose build --quiet
edgectl() {
  compose run --rm --no-deps -T --user "$E2E_UID:$E2E_GID" -v "$E2E_PKI:/out" \
    --entrypoint /usr/local/bin/edgectl hub "$@" >/dev/null
}
edgectl ca -name e2e -prefix hub-ca -out /out
edgectl ca -name e2e -prefix connector-ca -out /out
edgectl issue -ca /out -ca-prefix hub-ca -name hub -dns hub.test -server -out /out
edgectl token-key -out /out
# The token is the one file the connector reads from here, as UID 65532.
compose run --rm --no-deps -T -v "$E2E_PKI:/out" --entrypoint /usr/local/bin/edgectl hub \
  token -key /out/token.key -hub-ca /out/hub-ca.crt -identity connector-1 -expires 1h \
  2>/dev/null > "$E2E_PKI/token"
chmod 0644 "$E2E_PKI/token"

compose up --detach --wait postgres hub connector
connector_logged '"msg":"enrolled"' || fail "connector did not enroll"
pass "connector enrolls with the token, as its own non-root user"

out=$(client psql -h hub -tAc "select 'through the tunnel'")
[ "$out" = "through the tunnel" ] || fail "query through the hub returned: $out"
pass "client queries Postgres through the hub"

bytes=$(client sh -c "psql -h hub -c 'copy (select generate_series(1, 2000000)) to stdout' | wc -c")
[ "$bytes" -eq 14888896 ] || fail "bulk copy returned $bytes bytes"
pass "bulk copy of $bytes bytes arrives intact"

if client psql -h hub -p 5433 -tAc 'select 1' >/dev/null 2>&1; then
  fail "a target off the allowlist was reachable"
fi
connector_logged '"reason":"denied"' || fail "connector did not log the denial"
pass "target off the allowlist is refused, and the connector logs it"

if client pg_isready -h postgres -t 3 >/dev/null 2>&1; then
  fail "client reached Postgres directly"
fi
pass "client cannot reach the customer network directly"

# A new container, the same state volume, and no token.
: > "$E2E_PKI/token"
compose up --detach --wait --force-recreate --no-deps connector
connector_logged '"msg":"using enrolled certificate"' || fail "recreated connector did not reuse its state"
out=$(client psql -h hub -tAc "select 'after restart'")
[ "$out" = "after restart" ] || fail "query after restart returned: $out"
pass "recreated connector reconnects from its state volume without the token"

