#!/bin/bash
set -euo pipefail

while getopts "n:p:c:d:m:" opt; do
  case "$opt" in
    n) AGENT_NAME="$OPTARG" ;;
    p) AGENT_PORT="$OPTARG" ;;
    c) MAX_CONCURRENT="$OPTARG" ;;
    d) DATA_ROOT_BASE="$OPTARG" ;;
    m) MANAGER_HOST_OVERRIDE="$OPTARG" ;;
  esac
done

AGENT_NAME="${AGENT_NAME:-path-agent}"
AGENT_PORT="${AGENT_PORT:-$((30000 + RANDOM % 10001))}"
MAX_CONCURRENT="${MAX_CONCURRENT:-5}"
DATA_ROOT_BASE="${DATA_ROOT_BASE:-/opt/aspanel/path-agent}"
MANAGER_HOST_OVERRIDE="${MANAGER_HOST_OVERRIDE:-}"

sanitize_name() {
  local n
  n="$(echo "$1" | tr -cs 'a-zA-Z0-9._-' '-' | sed 's/^[._-]*//; s/[._-]*$//' | tr 'A-Z' 'a-z')"
  if [ -z "$n" ]; then
    n="path-agent"
  fi
  echo "$n"
}

check_port_free() {
  local port="$1"
  if $SUDO docker ps --format '{{.Ports}}' | grep -q ":${port}->"; then
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

if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
else
  SUDO="sudo"
fi

has_systemd() {
  command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]
}

detect_public_host() {
  local host=""
  host="$(curl -4fsSL https://api.ipify.org 2>/dev/null || true)"
  if [ -z "$host" ] && command -v ip >/dev/null 2>&1; then
    host="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if ($i=="src"){print $(i+1); exit}}')"
  fi
  if [ -z "$host" ]; then
    host="$(hostname -I 2>/dev/null | awk '{print $1}')"
  fi
  if [ -z "$host" ]; then
    echo "failed to determine public host automatically; pass -m <host>"
    exit 1
  fi
  printf '%s\n' "$host"
}

resolve_manager_host() {
  if [ -n "$MANAGER_HOST_OVERRIDE" ]; then
    printf '%s\n' "$MANAGER_HOST_OVERRIDE"
    return 0
  fi
  printf '%s\n' "$PUBLIC_HOST"
}

ensure_packages() {
  if ! command -v curl >/dev/null 2>&1; then
    $SUDO apt-get update
    $SUDO apt-get install -y curl
  fi
  if ! command -v sha256sum >/dev/null 2>&1; then
    $SUDO apt-get update
    $SUDO apt-get install -y coreutils
  fi
}

install_docker() {
  if command -v apt-get >/dev/null 2>&1; then
    $SUDO apt-get update
    $SUDO apt-get install -y ca-certificates curl
    curl -fsSL https://get.docker.com | $SUDO sh
    return 0
  fi
  curl -fsSL https://get.docker.com | $SUDO sh
}

ensure_docker() {
  if ! command -v docker >/dev/null 2>&1; then
    install_docker
  fi
  if ! $SUDO docker info >/dev/null 2>&1; then
    $SUDO systemctl start docker 2>/dev/null || $SUDO service docker start 2>/dev/null || true
  fi
  if ! $SUDO docker info >/dev/null 2>&1; then
    echo "docker not ready"
    exit 1
  fi
}

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
  $SUDO curl -fsSL "$url" -o "$tmp_file"
  $SUDO chmod +x "$tmp_file"
  $SUDO mv "$tmp_file" "$MANAGER_BIN_FILE"
}

json_escape() {
  printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

SAFE_NAME="$(sanitize_name "$AGENT_NAME")"
CONTAINER_NAME="path-agent-${SAFE_NAME}"
DATA_ROOT="${DATA_ROOT_BASE}/${SAFE_NAME}"
MANAGER_SERVICE_NAME="aspanel-docker-manager-path-${SAFE_NAME}"
MANAGER_SERVICE_FILE="/etc/systemd/system/${MANAGER_SERVICE_NAME}.service"
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
ensure_packages
ensure_docker

mkdir -p "$OUTPUT_DIR_HOST"
mkdir -p "$DATA_ROOT"

write_manager_service() {
  $SUDO tee "$MANAGER_SERVICE_FILE" >/dev/null <<EOF
[Unit]
Description=ASPanel Docker Manager (Path ${SAFE_NAME})
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=simple
WorkingDirectory=${DATA_ROOT}
ExecStart=${MANAGER_BIN_FILE} --port ${MANAGER_PORT} --token ${MANAGER_TOKEN} --config ${MANAGER_CONFIG_FILE} --state-file ${MANAGER_STATE_FILE}
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
}

stop_legacy_manager_pid() {
  if [ -f "$MANAGER_PID_FILE" ]; then
    OLD_PID="$(cat "$MANAGER_PID_FILE" 2>/dev/null || true)"
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" >/dev/null 2>&1; then
      kill "$OLD_PID" >/dev/null 2>&1 || true
    fi
  fi
}

start_manager() {
  stop_legacy_manager_pid
  if has_systemd; then
    write_manager_service
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable --now "$MANAGER_SERVICE_NAME" >/dev/null
    echo "systemd" > "$MANAGER_PID_FILE"
    return 0
  fi
  nohup "$MANAGER_BIN_FILE" --port "$MANAGER_PORT" --token "$MANAGER_TOKEN" --config "$MANAGER_CONFIG_FILE" --state-file "$MANAGER_STATE_FILE" >> "$MANAGER_LOG_FILE" 2>&1 &
  echo $! > "$MANAGER_PID_FILE"
}

wait_for_manager_health() {
  local url="$1"
  local attempt
  for attempt in $(seq 1 30); do
    if curl -fsS -H "X-Manager-Token: ${MANAGER_TOKEN}" "${url}/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "[!] docker-manager health check failed: ${url}/health"
  if has_systemd; then
    $SUDO systemctl status "$MANAGER_SERVICE_NAME" --no-pager || true
    $SUDO journalctl -u "$MANAGER_SERVICE_NAME" -n 50 --no-pager || true
  else
    tail -n 50 "$MANAGER_LOG_FILE" 2>/dev/null || true
  fi
  return 1
}

if [ -f "$TOKEN_FILE" ]; then
  API_TOKEN="$(cat "$TOKEN_FILE" | tr -d ' \t\r\n')"
else
  API_TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  echo "$API_TOKEN" > "$TOKEN_FILE"
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

PUBLIC_HOST="$(detect_public_host)"
MANAGER_HOST="$(resolve_manager_host)"
MANAGER_URL="http://${MANAGER_HOST}:${MANAGER_PORT}"

while ! check_port_free "$AGENT_PORT"; do
  echo "[!] Port $AGENT_PORT is already in use. Assigning a new random port..."
  AGENT_PORT="$((30000 + RANDOM % 10001))"
done

IMAGE="path-agent:${SAFE_NAME}"
$SUDO docker build --pull --no-cache -f "${TMP}/path-agent.Dockerfile" -t "$IMAGE" "$TMP"
$SUDO docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

$SUDO docker run -d \
  --name "$CONTAINER_NAME" \
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

cat > "$MANAGER_CONFIG_FILE" <<EOF
{"containers":["$CONTAINER_NAME"],"update_script":"$UPDATE_SCRIPT_FILE","update_log":"$UPDATE_LOG_FILE","command_timeout_sec":600}
EOF
install_manager_binary
{
  echo '#!/bin/bash'
  echo 'set -euo pipefail'
  echo 'sleep 1'
  printf 'MANAGER_ALLOW_REUSE_PORT=1 curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/path-agent-entrypoint.sh | bash -s -- -n %q -p %q -c %q -d %q' "$AGENT_NAME" "$AGENT_PORT" "$MAX_CONCURRENT" "$DATA_ROOT_BASE"
  if [ -n "$MANAGER_HOST_OVERRIDE" ]; then
    printf ' -m %q' "$MANAGER_HOST_OVERRIDE"
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
start_manager
if ! wait_for_manager_health "$MANAGER_URL"; then
  exit 1
fi

echo ""
echo "[*] Waiting for pathagent:// link..."
PROTO=""
for i in $(seq 1 20); do
  PROTO="$($SUDO docker logs "$CONTAINER_NAME" 2>/dev/null | grep -m1 'pathagent://' || true)"
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
  PREFIX="pathagent://"
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
  $SUDO docker logs --tail 80 "$CONTAINER_NAME"
fi
