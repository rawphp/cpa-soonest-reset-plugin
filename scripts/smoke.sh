#!/usr/bin/env bash
# Loads the plugin into a gateway built from the v8.0.51 source and checks it
# through the real gateway: registration, both capabilities, the pick order,
# failover on a 429, and a retest pass from the plugin's ticker.
#
# Everything runs on 127.0.0.1 ports picked at random, in a temp dir that is
# removed afterwards (KEEP=1 keeps it). Upstream traffic goes to
# scripts/fake_upstream.py; no real credentials are used.
#
#   CPA_SRC      directory of the CLIProxyAPI v8.0.51 source; required unless GATEWAY_BIN is set
#   GATEWAY_BIN  use this gateway binary instead of building one from CPA_SRC
set -euo pipefail

cd "$(dirname "$0")/.."
src=${CPA_SRC:-}
if [[ -z ${GATEWAY_BIN:-} ]]; then
	if [[ -z $src ]]; then
		echo "smoke.sh: set CPA_SRC to the CLIProxyAPI v8.0.51 source directory, or GATEWAY_BIN to a built gateway" >&2
		exit 2
	fi
	if [[ ! -d $src ]]; then
		echo "smoke.sh: CPA_SRC=$src is not a directory" >&2
		exit 2
	fi
	if [[ ! -f $src/go.mod || ! -d $src/cmd/server ]]; then
		echo "smoke.sh: CPA_SRC=$src does not look like the CLIProxyAPI source (no go.mod or cmd/server)" >&2
		exit 2
	fi
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/soonest-reset-smoke.XXXXXX")
pids=()

cleanup() {
	for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
	wait 2>/dev/null || true
	if [[ ${KEEP:-} == 1 ]]; then echo "kept $work"; else rm -rf "$work"; fi
}
trap cleanup EXIT

fail() {
	echo "FAIL: $*" >&2
	echo "--- gateway log (last 30 lines) ---" >&2
	tail -30 "$work/gateway.log" >&2 2>/dev/null || true
	exit 1
}
pass() { echo "ok   $*"; }

free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
utc() { date -u -r "$1" +%Y-%m-%dT%H:%M:%SZ; }

echo "building the plugin"
./scripts/build.sh >/dev/null
gateway=${GATEWAY_BIN:-}
if [[ -z $gateway ]]; then
	echo "building the gateway from $src"
	(cd "$src" && go build -trimpath -o "$work/cliproxyapi" ./cmd/server)
	gateway=$work/cliproxyapi
fi

mkdir -p "$work/auths" "$work/plugins/darwin/arm64"
cp plugins/darwin/arm64/soonest-reset.dylib "$work/plugins/darwin/arm64/"
# One inert Claude OAuth file, so host.auth.list has an entry for the retest to
# reset. It serves no smoke model and its made-up token never comes up for refresh.
cat >"$work/auths/claude-acct-c@example.test.json" <<'JSON'
{"type":"claude","email":"acct-c@example.test","access_token":"made-up-not-a-token","expired":"2099-01-01T00:00:00Z"}
JSON

now=$(date +%s)
reset_a=$((now + 5 * 86400))
reset_b=$((now + 2 * 86400))
python3 scripts/fake_upstream.py "$work/upstream.port" "sk-smoke-a=$reset_a" "sk-smoke-b=$reset_b" &
pids+=($!)
for _ in $(seq 50); do [[ -s $work/upstream.port ]] && break; sleep 0.1; done
[[ -s $work/upstream.port ]] || fail "fake upstream did not start"
upstream=http://127.0.0.1:$(cat "$work/upstream.port")

port=$(free_port)
gw=http://127.0.0.1:$port
cat >"$work/config.yaml" <<YAML
config-version: 8
server:
  host: "127.0.0.1"
  port: $port
management:
  allow-remote: false
  secret-key: "smoke-management-key"
  disable-control-panel: true
access:
  api-keys:
    - "smoke-client-key"
oauth:
  auth-dir: "$work/auths"
api-keys:
  claude:
    - name: claude-a
      base-url: "$upstream"
      models:
        - name: "claude-smoke"
      keys:
        - api-key: "sk-smoke-a"
    - name: claude-b
      base-url: "$upstream"
      models:
        - name: "claude-smoke"
      keys:
        - api-key: "sk-smoke-b"
observability:
  logs:
    debug: true
plugins:
  enabled: true
  dir: "$work/plugins"
  configs:
    soonest-reset:
      enabled: true
      priority: 1
      retest_minutes: 60
      log_picks: true
YAML

echo "starting the gateway on $gw"
(cd "$work" && exec "$gateway" -config "$work/config.yaml" -local-model >"$work/gateway.log" 2>&1) &
pids+=($!)

plugins=""
for _ in $(seq 60); do
	if plugins=$(curl -sf -H "Authorization: Bearer smoke-management-key" "$gw/v0/management/plugins"); then break; fi
	sleep 0.5
done
[[ -n $plugins ]] || fail "gateway did not answer"

# 1. The gateway loaded and registered the plugin.
grep -q "pluginhost: plugin registered plugin_id=soonest-reset" "$work/gateway.log" || fail "no registration line in the gateway log"
python3 - "$plugins" <<'PY' || fail "management API does not list soonest-reset as registered and enabled"
import json, sys
entry = {p["id"]: p for p in json.loads(sys.argv[1])["plugins"]}.get("soonest-reset")
sys.exit(0 if entry and entry["registered"] and entry["effective_enabled"] else 1)
PY
pass "plugin loaded and registered"

ask() {
	curl -s -o /dev/null -w "%{http_code}" -X POST "$gw/v1/messages" \
		-H "x-api-key: smoke-client-key" -H "content-type: application/json" -H "anthropic-version: 2023-06-01" \
		-d '{"model":"claude-smoke","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
}
served() { curl -s "$upstream/served"; }
picks() { grep -o "soonest-reset: pick .*" "$work/gateway.log" || true; }

# 2. A token count goes through account selection but publishes no usage
#    record, so it must not use up a probe.
code=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$gw/v1/messages/count_tokens" \
	-H "x-api-key: smoke-client-key" -H "content-type: application/json" -H "anthropic-version: 2023-06-01" \
	-d '{"model":"claude-smoke","messages":[{"role":"user","content":"hi"}]}')
[[ $code == 200 ]] || fail "token count answered $code"
picks | sed -n '1p' | grep -q "rule=soonest-reset reset_7d=none" || fail "the token count pick was not a plain pick: $(picks)"
pass "a token count is picked without spending a probe"

# 3. Two probes, then the soonest weekly reset. The pick lines come from
#    scheduler.pick; the reset time in them comes from usage.handle.
for i in 1 2 3 4; do
	code=$(ask)
	[[ $code == 200 ]] || fail "request $i answered $code"
	sleep 1
done
python3 - "$(served)" <<'PY' || fail "upstream saw $(served); want a and b once each, then b, b"
import json, sys
s = json.loads(sys.argv[1])
sys.exit(0 if sorted(s[:2]) == ["sk-smoke-a", "sk-smoke-b"] and s[2:] == ["sk-smoke-b", "sk-smoke-b"] else 1)
PY
picks | sed -n '2,3p' | grep -c "rule=probe reset_7d=none" | grep -qx 2 || fail "first two picks were not probes: $(picks)"
picks | sed -n '4,5p' | grep -c "rule=soonest-reset reset_7d=$(utc "$reset_b")" | grep -qx 2 ||
	fail "picks 4-5 did not follow the soonest reset $(utc "$reset_b"): $(picks)"
pass "scheduler and usage capabilities: probes, then the soonest reset"

# 4. A 429 on the soonest account moves the same request to the other one.
curl -sf -X POST "$upstream/control" -d '{"key":"sk-smoke-b","status":429}' >/dev/null
code=$(ask)
[[ $code == 200 ]] || fail "request after the 429 answered $code"
sleep 1
code=$(ask)
[[ $code == 200 ]] || fail "second request after the 429 answered $code"
python3 - "$(served)" <<'PY' || fail "upstream saw $(served); want b (429), a, then a"
import json, sys
sys.exit(0 if json.loads(sys.argv[1])[4:] == ["sk-smoke-b", "sk-smoke-a", "sk-smoke-a"] else 1)
PY
pass "a 429 fails over to the next account, and the cooled account is skipped"

# 5. The first retest runs at the ticker's first tick, a minute after
#    registration, though retest_minutes is 60. It resets the Claude OAuth
#    account through host.auth.list and host.routing.reset_cooldown.
echo "waiting for the first retest (up to 90s)"
for _ in $(seq 90); do
	grep -q "soonest-reset: retest" "$work/gateway.log" && break
	sleep 1
done
grep -q "soonest-reset: retest reset=1 probe=\[\]$" "$work/gateway.log" || fail "no clean retest line: $(grep -o 'soonest-reset: retest.*' "$work/gateway.log" || true)"
grep -q "pluginhost: plugin reset credential cooldown plugin_id=soonest-reset" "$work/gateway.log" || fail "the gateway did not log the cooldown reset"
pass "the first retest ran at startup and reset the Claude OAuth account"

if grep -Eiq "fused|panic" "$work/gateway.log"; then fail "the gateway log mentions a panic or a fused plugin"; fi
pass "no panic and no fused plugin"
echo "smoke passed"
