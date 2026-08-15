#!/usr/bin/env bash
# The Fleetplane MVP demo (docs/08 §7), self-verifying:
#
#   1. acquire            → a machine is created and bound
#   2. acquire again      → the SAME machine is reused (no second create)
#   3. acquire again      → capacity exhausted → a second machine
#   4. release everything → idle reclaim deletes the machines by policy
#   5. kill -9 mid-create → restart → journal recovery converges to
#                           exactly one machine (invariant 7, live)
#
# Runs against the deterministic fake provider by default. To run against
# real Hetzner: point FLEETPLANE_DEMO_CONFIG at a config whose provider is
# hetzner (dedicated test project! see docs/adr/ADR-015-e2e-safety.md).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
bin="$work/fleetplane"
db="$work/demo.db"
addr="127.0.0.1:18280"
export FLEETPLANE_ADDR="http://$addr"

cleanup() {
    [[ -n "${server_pid:-}" ]] && kill "$server_pid" 2>/dev/null || true
    wait 2>/dev/null || true
    rm -rf "$work"
}
trap cleanup EXIT

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { printf '\033[31mDEMO FAILED: %s\033[0m\n' "$*" >&2; exit 1; }

machine_count() { "$bin" resources | tail -n +2 | grep -c . || true; }
fleet_empty()   { [[ "$(machine_count)" -eq 0 ]]; }

wait_until() { # seconds, description, command...
    local deadline=$(( $(date +%s) + $1 )); shift
    local desc="$1"; shift
    until "$@" >/dev/null 2>&1; do
        (( $(date +%s) > deadline )) && fail "timed out waiting for: $desc"
        sleep 0.3
    done
}

start_server() {
    "$bin" serve --config "$cfg" --log-level warn 2>>"$work/server.log" &
    server_pid=$!
    wait_until 15 "server readiness" curl -sf "http://$addr/health/ready"
}

say "building fleetplane"
go build -o "$bin" github.com/samishal1998/fleetplane/cmd/fleetplane

cfg="${FLEETPLANE_DEMO_CONFIG:-$work/config.yaml}"
if [[ ! -f "$cfg" ]]; then
    cat > "$cfg" <<EOF
server: { addr: "$addr", opsAddr: "127.0.0.1:19280", shutdownGrace: 2s }
storage: { path: $db }
engine: { pollInterval: 300ms, verifyWindow: 5s }
reconcile: { interval: 1s }
providers:
  demo:
    driver: fake
    settings: { createSteps: 2 }
classes:
  ci-large:
    kind: compute.machine
    provider: demo
    spec: { serverType: cpx31, image: "snapshot:ci-runner=v12" }
    reclaim: { idleAfter: 3s }
EOF
fi

say "starting fleetplane"
start_server

say "1) acquire: no capacity exists — a machine is created"
acq1=$("$bin" acquire --class ci-large --cpu 1 --ttl 30m | head -1 | awk '{print $1}')
"$bin" watch "$acq1" --interval 300ms --timeout 60s
[[ $(machine_count) -eq 1 ]] || fail "expected 1 machine after first acquire, got $(machine_count)"

say "2) acquire again: the existing machine is REUSED (08 §7: reuse before create)"
acq2=$("$bin" acquire --class ci-large --cpu 1 | head -1 | awk '{print $1}')
"$bin" watch "$acq2" --interval 300ms --timeout 60s
[[ $(machine_count) -eq 1 ]] || fail "reuse failed: $(machine_count) machines after second acquire"
echo "   ✓ still exactly 1 machine — capacity was reused"

say "3) acquire once more: capacity exhausted — a second machine is created"
acq3=$("$bin" acquire --class ci-large --cpu 1 | head -1 | awk '{print $1}')
"$bin" watch "$acq3" --interval 300ms --timeout 60s
[[ $(machine_count) -eq 2 ]] || fail "expected 2 machines, got $(machine_count)"

say "current fleet"
"$bin" resources

say "4) release everything: idle policy reclaims the machines (class idleAfter=3s)"
"$bin" release "$acq1"; "$bin" release "$acq2"; "$bin" release "$acq3"
wait_until 60 "idle reclaim to delete both machines" fleet_empty
echo "   ✓ fleet is empty again"

say "5) kill -9 during create, then restart: recovery must not duplicate (invariant 7)"
acq4=$("$bin" acquire --class ci-large --cpu 1 | head -1 | awk '{print $1}')
sleep 0.5                      # the create is mid-flight
kill -9 "$server_pid"; wait "$server_pid" 2>/dev/null || true
echo "   server killed with SIGKILL mid-provisioning"
start_server
echo "   server restarted; journal recovery running"
"$bin" watch "$acq4" --interval 300ms --timeout 90s
[[ $(machine_count) -eq 1 ]] || fail "recovery produced $(machine_count) machines, want exactly 1"
echo "   ✓ exactly one machine after crash recovery"
"$bin" release "$acq4"

say "DEMO PASSED"
