#!/usr/bin/env bash
# test-wsl-keyring.sh
# Smoke test for wsl-keyring: verifies the D-Bus Secret Service is functional
# using secret-tool (libsecret-tools).
#
# Usage:
#   ./scripts/test-wsl-keyring.sh
#
# Requirements:
#   - secret-tool  (apt install libsecret-tools)
#   - An active D-Bus session bus (DBUS_SESSION_BUS_ADDRESS)

set -euo pipefail

# ── colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}[PASS]${NC} $*"; }
fail() { echo -e "${RED}[FAIL]${NC} $*"; EXIT_CODE=1; }
info() { echo -e "${YELLOW}[INFO]${NC} $*"; }

EXIT_CODE=0
DAEMON_PID=""
BINARY="$(mktemp -t wsl-keyring-XXXXXX)"

cleanup() {
  if [[ -n "$DAEMON_PID" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    info "Stopping wsl-keyring (pid=$DAEMON_PID)..."
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  rm -f "$BINARY"
}
trap cleanup EXIT

# ── 1. Prerequisites ─────────────────────────────────────────────────────────
echo "=== wsl-keyring smoke test ==="
echo

if ! command -v secret-tool &>/dev/null; then
  echo "secret-tool not found. Install it with:"
  echo "  sudo apt install libsecret-tools"
  exit 1
fi

if [[ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]]; then
  fail "DBUS_SESSION_BUS_ADDRESS is not set. Is a D-Bus session running?"
  exit 1
fi

# ── 2. Build ─────────────────────────────────────────────────────────────────
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
info "Building wsl-keyring from $REPO_ROOT ..."
go build -o "$BINARY" "$REPO_ROOT/cmd/wsl-keyring"
pass "Build succeeded"

# ── 3. Start daemon (InMemory backend) ───────────────────────────────────────
info "Starting wsl-keyring daemon (USE_IN_MEMORY=true) ..."
USE_IN_MEMORY=true "$BINARY" &>/tmp/wsl-keyring-test.log &
DAEMON_PID=$!

# Wait until the service is registered on D-Bus (up to 5 seconds)
for i in $(seq 1 10); do
  if dbus-send --session --print-reply \
       --dest=org.freedesktop.DBus \
       /org/freedesktop/DBus \
       org.freedesktop.DBus.ListNames 2>/dev/null | grep -q "org.freedesktop.secrets"; then
    break
  fi
  sleep 0.5
  if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
    fail "wsl-keyring exited unexpectedly. Logs:"
    cat /tmp/wsl-keyring-test.log
    exit 1
  fi
done

if ! dbus-send --session --print-reply \
     --dest=org.freedesktop.DBus \
     /org/freedesktop/DBus \
     org.freedesktop.DBus.ListNames 2>/dev/null | grep -q "org.freedesktop.secrets"; then
  fail "wsl-keyring did not register on D-Bus within 5 s"
  info "Daemon logs:"
  cat /tmp/wsl-keyring-test.log
  exit 1
fi
pass "wsl-keyring registered on D-Bus (pid=$DAEMON_PID)"

# ── 4. Store a secret ────────────────────────────────────────────────────────
TEST_SECRET="test-secret-$(date +%s)"
TEST_LABEL="wsl-keyring smoke test"
TEST_ATTR_KEY="application"
TEST_ATTR_VAL="wsl-keyring-test"

info "Storing secret via secret-tool ..."
if echo "$TEST_SECRET" | secret-tool store \
    --label="$TEST_LABEL" \
    "$TEST_ATTR_KEY" "$TEST_ATTR_VAL" 2>/tmp/secret-tool-store.log; then
  pass "secret-tool store succeeded"
else
  fail "secret-tool store failed:"
  cat /tmp/secret-tool-store.log
  exit 1
fi

# ── 5. Lookup the secret ─────────────────────────────────────────────────────
info "Looking up secret via secret-tool ..."
RESULT=$(secret-tool lookup "$TEST_ATTR_KEY" "$TEST_ATTR_VAL" 2>/tmp/secret-tool-lookup.log || true)

if [[ "$RESULT" == "$TEST_SECRET" ]]; then
  pass "secret-tool lookup returned correct value"
else
  fail "secret-tool lookup returned unexpected value"
  echo "  Expected : $TEST_SECRET"
  echo "  Got      : $RESULT"
  info "Lookup logs:"
  cat /tmp/secret-tool-lookup.log
fi

# ── 6. Search by attributes ──────────────────────────────────────────────────
info "Searching items via secret-tool search ..."
if secret-tool search "$TEST_ATTR_KEY" "$TEST_ATTR_VAL" 2>/tmp/secret-tool-search.log | grep -q "$TEST_LABEL"; then
  pass "secret-tool search found the item"
else
  fail "secret-tool search did not find the item"
  cat /tmp/secret-tool-search.log
fi

# ── 7. Clear (delete) the secret ─────────────────────────────────────────────
info "Deleting secret via secret-tool clear ..."
if secret-tool clear "$TEST_ATTR_KEY" "$TEST_ATTR_VAL" 2>/tmp/secret-tool-clear.log; then
  pass "secret-tool clear succeeded"
else
  fail "secret-tool clear failed:"
  cat /tmp/secret-tool-clear.log
fi

# Confirm it's gone
AFTER=$(secret-tool lookup "$TEST_ATTR_KEY" "$TEST_ATTR_VAL" 2>/dev/null || true)
if [[ -z "$AFTER" ]]; then
  pass "Secret was deleted (lookup returns empty)"
else
  fail "Secret still present after clear: $AFTER"
fi

# ── Summary ──────────────────────────────────────────────────────────────────
echo
if [[ $EXIT_CODE -eq 0 ]]; then
  echo -e "${GREEN}All tests passed.${NC}"
else
  echo -e "${RED}Some tests failed. Check output above.${NC}"
fi

exit $EXIT_CODE
