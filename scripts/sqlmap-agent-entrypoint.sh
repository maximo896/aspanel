#!/bin/bash
set -euo pipefail

while getopts "n:p:c:l:d:" opt; do
  case "$opt" in
    n) AGENT_NAME="$OPTARG" ;;
    p) AGENT_PORT="$OPTARG" ;;
    c) MAX_CONCURRENT="$OPTARG" ;;
    l) PROXY_AGENT_LINK="$OPTARG" ;;
    d) DATA_ROOT_BASE="$OPTARG" ;;
  esac
done

AGENT_NAME="${AGENT_NAME:-agent}"
AGENT_PORT="${AGENT_PORT:-5000}"
MAX_CONCURRENT="${MAX_CONCURRENT:-10}"
PROXY_AGENT_LINK="${PROXY_AGENT_LINK:-}"
DATA_ROOT_BASE="${DATA_ROOT_BASE:-/opt/aspanel/sqlmap-agent}"

sanitize_name() {
  local n
  n="$(echo "$1" | tr -cs 'a-zA-Z0-9._-' '-' | sed 's/^[._-]*//; s/[._-]*$//' | tr 'A-Z' 'a-z')"
  if [ -z "$n" ]; then
    n="agent"
  fi
  echo "$n"
}

if ! command -v curl >/dev/null 2>&1; then
  apt-get update && apt-get install -y curl
fi
if ! command -v docker >/dev/null 2>&1; then
  curl -fsSL https://get.docker.com | sh
fi
if ! docker info >/dev/null 2>&1; then
  systemctl start docker 2>/dev/null || service docker start 2>/dev/null || true
fi
if ! docker info >/dev/null 2>&1; then
  echo "docker not ready"
  exit 1
fi

SAFE_NAME="$(sanitize_name "$AGENT_NAME")"
NETWORK_NAME="scan-net-${SAFE_NAME}"
SQLMAP_CN="sqlmap-agent-${SAFE_NAME}"
GATEWAY_CN="proxy-gateway-${SAFE_NAME}"
DATA_ROOT="${DATA_ROOT_BASE}/${SAFE_NAME}"
OUTPUT_DIR_HOST="${DATA_ROOT}/output"
TOKEN_FILE="${DATA_ROOT}/api_token"
mkdir -p "$OUTPUT_DIR_HOST"
mkdir -p "$DATA_ROOT"

if [ -f "$TOKEN_FILE" ]; then
  API_TOKEN="$(cat "$TOKEN_FILE" | tr -d ' \t\r\n')"
else
  API_TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  echo "$API_TOKEN" > "$TOKEN_FILE"
fi

if [ -z "$API_TOKEN" ]; then
  echo "failed to initialize api token"
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl -fsSL https://github.com/maximo896/as/archive/refs/heads/main.tar.gz | tar xz -C "$TMP" --strip-components=1

PUBLIC_HOST=$(curl -fsSL https://api.ipify.org 2>/dev/null || true)
if [ -z "$PUBLIC_HOST" ]; then
  PUBLIC_HOST=$(hostname -I | awk '{print $1}')
fi

IMAGE="sqlmap-agent:${SAFE_NAME}"
docker build --pull --no-cache -t "$IMAGE" "$TMP"
docker network create "$NETWORK_NAME" >/dev/null 2>&1 || true
docker rm -f "$SQLMAP_CN" >/dev/null 2>&1 || true

docker run -d \
  --name "$SQLMAP_CN" \
  --network "$NETWORK_NAME" \
  -p "${AGENT_PORT}:5000" \
  -e AGENT_NAME="$AGENT_NAME" \
  -e MAX_CONCURRENT="$MAX_CONCURRENT" \
  -e PUBLIC_HOST="$PUBLIC_HOST" \
  -e HOST_PORT="$AGENT_PORT" \
  -e API_TOKEN="$API_TOKEN" \
  -e OUTPUT_DIR="/app/output" \
  -v "${OUTPUT_DIR_HOST}:/app/output" \
  --restart always \
  "$IMAGE" >/dev/null

if [ -n "$PROXY_AGENT_LINK" ]; then
  curl -fsSL https://github.com/maximo896/as/raw/refs/heads/main/proxy-gateway-entrypoint.sh | \
    bash -s -- -n "$AGENT_NAME" -l "$PROXY_AGENT_LINK" -N "$NETWORK_NAME"
fi

echo ""
echo "[*] Waiting for sqlmapagent:// link..."
PROTO=""
for i in $(seq 1 20); do
  PROTO="$(docker logs "$SQLMAP_CN" 2>/dev/null | grep -m1 'sqlmapagent://' || true)"
  if [ -n "$PROTO" ]; then
    break
  fi
  sleep 1
done

echo ""
echo "=========================================="
echo "[+] Install/Update Complete!"
echo "=========================================="
echo "[+] Persistent output dir: $OUTPUT_DIR_HOST"
echo ""
if [ -n "$PROTO" ]; then
  echo "$PROTO"
else
  echo "[!] Protocol link not found in logs, showing last 80 lines:"
  docker logs --tail 80 "$SQLMAP_CN"
fi
