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

check_port_free() {
  local port="$1"
  if docker ps --format '{{.Ports}}' | grep -q ":${port}->"; then
    return 1
  fi
  if command -v nc >/dev/null 2>&1; then
    if nc -z 127.0.0.1 "$port" >/dev/null 2>&1; then
      return 1
    fi
  else
    if (echo >/dev/tcp/127.0.0.1/"$port") >/dev/null 2>&1; then
      return 1
    fi
  fi
  return 0
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
MANAGER_TOKEN_FILE="${DATA_ROOT}/manager_token"
MANAGER_PORT_FILE="${DATA_ROOT}/manager_port"
MANAGER_CONFIG_FILE="${DATA_ROOT}/manager_config.json"
MANAGER_BIN_FILE="${DATA_ROOT}/docker-manager"
MANAGER_PID_FILE="${DATA_ROOT}/docker-manager.pid"
MANAGER_STATE_FILE="${DATA_ROOT}/docker-manager.state.json"
MANAGER_LOG_FILE="${DATA_ROOT}/docker-manager.log"
UPDATE_SCRIPT_FILE="${DATA_ROOT}/update-agent.sh"
UPDATE_LOG_FILE="${DATA_ROOT}/update-agent.log"
mkdir -p "$OUTPUT_DIR_HOST"
mkdir -p "$DATA_ROOT"

detect_manager_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      echo "amd64"
      ;;
    aarch64|arm64)
      echo "arm64"
      ;;
    *)
      echo ""
      ;;
  esac
}

install_manager_binary() {
  local arch
  local url
  local tmp_file
  arch="$(detect_manager_arch)"
  if [ -z "$arch" ]; then
    echo "unsupported architecture: $(uname -m)"
    exit 1
  fi
  url="https://github.com/maximo896/aspanel/releases/latest/download/docker-manager-linux-${arch}"
  tmp_file="${MANAGER_BIN_FILE}.new"
  curl -fsSL "$url" -o "$tmp_file"
  chmod +x "$tmp_file"
  mv "$tmp_file" "$MANAGER_BIN_FILE"
}

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

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

if [ -f "$MANAGER_TOKEN_FILE" ]; then
  MANAGER_TOKEN="$(cat "$MANAGER_TOKEN_FILE" | tr -d ' \t\r\n')"
else
  MANAGER_TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  echo "$MANAGER_TOKEN" > "$MANAGER_TOKEN_FILE"
fi

MANAGER_PORT=""
if [ -f "$MANAGER_PORT_FILE" ]; then
  MANAGER_PORT="$(cat "$MANAGER_PORT_FILE" | tr -d ' \t\r\n')"
fi
if ! echo "$MANAGER_PORT" | grep -Eq '^[0-9]+$'; then
  MANAGER_PORT=""
fi
if [ -n "$MANAGER_PORT" ] && [ "${MANAGER_ALLOW_REUSE_PORT:-0}" != "1" ] && ! check_port_free "$MANAGER_PORT"; then
  MANAGER_PORT=""
fi
while [ -z "$MANAGER_PORT" ]; do
  CANDIDATE_PORT="$((30000 + RANDOM % 10001))"
  if check_port_free "$CANDIDATE_PORT"; then
    MANAGER_PORT="$CANDIDATE_PORT"
  fi
done
echo "$MANAGER_PORT" > "$MANAGER_PORT_FILE"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl -fsSL https://github.com/maximo896/as/archive/refs/heads/main.tar.gz | tar xz -C "$TMP" --strip-components=1

PUBLIC_HOST=$(curl -fsSL https://api.ipify.org 2>/dev/null || true)
if [ -z "$PUBLIC_HOST" ]; then
  PUBLIC_HOST=$(hostname -I | awk '{print $1}')
fi
MANAGER_URL="http://${PUBLIC_HOST}:${MANAGER_PORT}"

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

cat > "$MANAGER_CONFIG_FILE" <<EOF
{"containers":["$SQLMAP_CN"$( [ -n "$PROXY_AGENT_LINK" ] && printf ',"%s"' "$GATEWAY_CN" )],"update_script":"$UPDATE_SCRIPT_FILE","update_log":"$UPDATE_LOG_FILE","command_timeout_sec":600}
EOF
install_manager_binary
{
  echo '#!/bin/bash'
  echo 'set -euo pipefail'
  echo 'sleep 1'
  printf 'MANAGER_ALLOW_REUSE_PORT=1 curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/sqlmap-agent-entrypoint.sh | bash -s -- -n %q -p %q -c %q -d %q' "$AGENT_NAME" "$AGENT_PORT" "$MAX_CONCURRENT" "$DATA_ROOT_BASE"
  if [ -n "$PROXY_AGENT_LINK" ]; then
    printf ' -l %q' "$PROXY_AGENT_LINK"
  fi
  echo
} > "$UPDATE_SCRIPT_FILE"
chmod +x "$UPDATE_SCRIPT_FILE"
if [ -f "$MANAGER_PID_FILE" ]; then
  OLD_PID="$(cat "$MANAGER_PID_FILE" 2>/dev/null || true)"
  if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" >/dev/null 2>&1; then
    kill "$OLD_PID" >/dev/null 2>&1 || true
  fi
fi
nohup "$MANAGER_BIN_FILE" --port "$MANAGER_PORT" --token "$MANAGER_TOKEN" --config "$MANAGER_CONFIG_FILE" --state-file "$MANAGER_STATE_FILE" >> "$MANAGER_LOG_FILE" 2>&1 &
echo $! > "$MANAGER_PID_FILE"

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
  PREFIX="sqlmapagent://"
  RAW_PROTO="${PROTO#${PREFIX}}"
  DECODED_PROTO="$(printf '%s' "$RAW_PROTO" | base64 -d 2>/dev/null || true)"
  if [ -n "$DECODED_PROTO" ] && [ "$DECODED_PROTO" != "$RAW_PROTO" ]; then
    LAST_CHAR="${DECODED_PROTO#"${DECODED_PROTO%?}"}"
    if [ "$LAST_CHAR" = "}" ]; then
      UPDATED_PROTO="${DECODED_PROTO%?},\"manager_url\":\"$(json_escape "$MANAGER_URL")\",\"manager_token\":\"$(json_escape "$MANAGER_TOKEN")\"}"
      printf '%s%s\n' "$PREFIX" "$(printf '%s' "$UPDATED_PROTO" | base64 | tr -d '\n')"
    else
      echo "$PROTO"
    fi
  else
    echo "$PROTO"
  fi
else
  echo "[!] Protocol link not found in logs, showing last 80 lines:"
  docker logs --tail 80 "$SQLMAP_CN"
fi
