#!/usr/bin/env bash
# scripts/seed-demo.sh
#
# Start lilmail in demo mode and (optionally) capture screenshots.
#
# Demo mode uses an in-memory mail client seeded with realistic messages — no
# IMAP server, no real credentials. The binary is configured via a temporary
# config.toml written to a temp directory so the real config is never touched.
#
# Usage:
#   scripts/seed-demo.sh              # start server in foreground (Ctrl-C to stop)
#   scripts/seed-demo.sh --screenshots # start, capture screenshots, then stop
#
# Requirements:
#   - Go toolchain (to build the binary if missing)
#   - Node 18+ and 'npm install' run once in scripts/ (for --screenshots)
#   - Playwright Chromium: cd scripts && npx playwright install chromium

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
BINARY="$REPO/lilmail"
TMP_DIR="$REPO/.demo-tmp"
TMP_CFG="$TMP_DIR/config.toml"
PORT="${LILMAIL_PORT:-3099}"
BASE_URL="http://localhost:$PORT"

DEMO_EMAIL="${LILMAIL_DEMO_EMAIL:-demo@lilmail.dev}"
DEMO_PASSWORD="${LILMAIL_DEMO_PASSWORD:-demo}"

# Reproducible secrets (safe for local use; not for production).
JWT_SECRET="${LILMAIL_JWT_SECRET:-lilmail-demo-jwt-secret-not-for-production}"
ENC_KEY="${LILMAIL_ENC_KEY:-lilmail-demo-enc-key-32-bytes!!!}"  # exactly 32 chars

DO_SCREENSHOTS=0
if [[ "${1:-}" == "--screenshots" ]]; then
  DO_SCREENSHOTS=1
fi

# -----------------------------------------------------------------------
# Build binary if needed
# -----------------------------------------------------------------------
if [[ ! -f "$BINARY" ]]; then
  echo "[seed-demo] Building lilmail binary..."
  cd "$REPO" && go build -o lilmail .
fi

# -----------------------------------------------------------------------
# Write temporary config
# -----------------------------------------------------------------------
mkdir -p "$TMP_DIR"
cat > "$TMP_CFG" <<TOML
[server]
port = $PORT
username_is_email = true

[imap]
server = "imap.example.com"
port   = 993

[smtp]
server = "smtp.example.com"
port   = 587
use_starttls = true

[cache]
folder = "$TMP_DIR/cache"

[jwt]
secret = "$JWT_SECRET"

[encryption]
key = "${ENC_KEY:0:32}"

[demo]
enabled  = true
email    = "$DEMO_EMAIL"
password = "$DEMO_PASSWORD"
TOML

echo "[seed-demo] Demo config written to $TMP_CFG"
echo "[seed-demo] Demo login URL (no credentials needed): $BASE_URL/demo-login"
echo "[seed-demo] Starting lilmail on $BASE_URL ..."

# -----------------------------------------------------------------------
# Start server
# -----------------------------------------------------------------------
SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_DIR"
  echo "[seed-demo] Stopped."
}
trap cleanup EXIT INT TERM

# Run binary from TMP_DIR so it reads config.toml there
cd "$TMP_DIR"
"$BINARY" &
SERVER_PID=$!

# Wait for health check
DEADLINE=$((SECONDS + 15))
while [[ $SECONDS -lt $DEADLINE ]]; do
  if curl -sf "$BASE_URL/health" > /dev/null 2>&1; then
    break
  fi
  sleep 0.3
done

if ! curl -sf "$BASE_URL/health" > /dev/null 2>&1; then
  echo "[seed-demo] ERROR: server did not become ready within 15 s"
  exit 1
fi

echo "[seed-demo] Server ready."
echo "[seed-demo] Demo login URL: $BASE_URL/demo-login"

# -----------------------------------------------------------------------
# Screenshots (optional)
# -----------------------------------------------------------------------
if [[ $DO_SCREENSHOTS -eq 1 ]]; then
  echo "[seed-demo] Capturing screenshots..."
  cd "$REPO"

  if [[ ! -d "scripts/node_modules" ]]; then
    echo "[seed-demo] Installing Playwright..."
    cd scripts && npm install && cd ..
  fi

  LILMAIL_EXTERNAL=1 \
  BASE_URL="$BASE_URL" \
  LILMAIL_USERNAME="$DEMO_EMAIL" \
  LILMAIL_PASSWORD="$DEMO_PASSWORD" \
  LILMAIL_IMAP_SERVER="imap.example.com" \
  node scripts/demo-screenshots.mjs

  echo "[seed-demo] Screenshots written to docs/screenshots/"
else
  echo "[seed-demo] Server running. Press Ctrl-C to stop."
  wait "$SERVER_PID"
fi
