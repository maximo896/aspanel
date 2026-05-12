#!/bin/bash
set -euo pipefail

while getopts "n:p:c:" opt; do
  case "$opt" in
    n) AGENT_NAME="$OPTARG" ;;
    p) AGENT_PORT="$OPTARG" ;;
    c) MAX_CONCURRENT="$OPTARG" ;;
  esac
done

AGENT_NAME="${AGENT_NAME:-awvs}"
AGENT_PORT="${AGENT_PORT:-$((30000 + RANDOM % 10001))}"
MAX_CONCURRENT="${MAX_CONCURRENT:-5}"

sanitize_name() {
  local n
  n="$(echo "$1" | tr -cs 'a-zA-Z0-9._-' '-' | sed 's/^[._-]*//; s/[._-]*$//' | tr 'A-Z' 'a-z')"
  if [ -z "$n" ]; then
    n="awvs"
  fi
  echo "$n"
}

check_port_free() {
  local port="$1"
  # Check whether Docker already binds this port.
  if docker ps --format '{{.Ports}}' | grep -q ":${port}->"; then
    return 1
  fi
  # Check whether the host already listens on this port.
  if command -v nc >/dev/null 2>&1; then
    if nc -z 127.0.0.1 "$port" >/dev/null 2>&1; then
      return 1
    fi
  else
    # fallback to bash /dev/tcp
    if (echo >/dev/tcp/127.0.0.1/"$port") >/dev/null 2>&1; then
      return 1
    fi
  fi
  return 0
}

AWVS_EMAIL="admin@admin.com"
AWVS_PASSWORD="Admin123"
AWVS_CONTAINER_PORT="3443"
IMAGE="secfa/awvs:latest"

if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
else
  SUDO="sudo"
fi

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
  for pkg in docker.io docker-doc docker-compose docker-compose-v2 podman-docker containerd runc; do
    $SUDO apt-get remove -y "$pkg" >/dev/null 2>&1 || true
  done

  $SUDO apt-get update
  $SUDO apt-get install -y ca-certificates curl
  $SUDO install -m 0755 -d /etc/apt/keyrings
  $SUDO curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  $SUDO chmod a+r /etc/apt/keyrings/docker.asc

  ARCH="$(dpkg --print-architecture)"
  CODENAME="$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")"
  echo "deb [arch=${ARCH} signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${CODENAME} stable" | $SUDO tee /etc/apt/sources.list.d/docker.list >/dev/null
  $SUDO apt-get update
  $SUDO apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
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

extract_json_value() {
  local key="$1"
  local file="$2"
  tr -d '\n' < "$file" | sed -n "s/.*\"${key}\":[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p"
}

extract_first_json_string_value() {
  local file="$1"
  shift
  local key
  local value
  for key in "$@"; do
    value="$(extract_json_value "$key" "$file")"
    if [ -n "$value" ] && [ "$value" != "null" ]; then
      printf '%s\n' "$value"
      return 0
    fi
  done
  return 1
}

login_awvs() {
  local base_url="$1"
  local workdir="$2"
  local password_hash

  password_hash="$(printf '%s' "$AWVS_PASSWORD" | sha256sum | awk '{print $1}')"
  printf '{"email":"%s","password":"%s"}' "$AWVS_EMAIL" "$password_hash" > "${workdir}/login.json"

  curl -k -sS -D "${workdir}/login.headers" -c "${workdir}/cookies.txt" \
    -H 'Content-Type: application/json' \
    -X POST "${base_url}/api/v1/me/login" \
    --data-binary @"${workdir}/login.json" >/dev/null

  awk -F': ' 'BEGIN{IGNORECASE=1} /^X-Auth:/ {gsub("\r","",$2); print $2}' "${workdir}/login.headers" | tail -n 1
}

graphql_request() {
  local base_url="$1"
  local auth_token="$2"
  local workdir="$3"
  local body_file="$4"
  local out_file="$5"
  local endpoint
  local http_code
  local tmp_out

  tmp_out="${out_file}.tmp"
  for endpoint in "/graphql/" "/graphql" "/api/graphql/" "/api/graphql"; do
    http_code="$(curl -k -sS -b "${workdir}/cookies.txt" \
      -H "X-Auth: ${auth_token}" \
      -H 'Content-Type: application/json' \
      -X POST "${base_url}${endpoint}" \
      --data-binary @"${body_file}" \
      -o "${tmp_out}" \
      -w '%{http_code}' || true)"

    if [ "${http_code}" = "200" ] || [ "${http_code}" = "201" ]; then
      mv "${tmp_out}" "${out_file}"
      return 0
    fi
  done

  rm -f "${tmp_out}"
  : > "${out_file}"
  return 1
}

get_api_key_from_rest() {
  local base_url="$1"
  local auth_token="$2"
  local workdir="$3"
  local out_file="${workdir}/me.json"
  local http_code

  http_code="$(curl -k -sS -b "${workdir}/cookies.txt" \
    -H "X-Auth: ${auth_token}" \
    -H 'Accept: application/json' \
    -X GET "${base_url}/api/v1/me" \
    -o "${out_file}" \
    -w '%{http_code}' || true)"

  if [ "${http_code}" != "200" ]; then
    return 1
  fi

  extract_first_json_string_value "${out_file}" "api_key" "apiKey" "apikey"
}

wait_for_awvs() {
  local base_url="$1"
  local attempts=120

  echo "[*] Waiting for AWVS to become ready..."
  for _ in $(seq 1 "$attempts"); do
    code="$(curl -k -s -o /dev/null -w '%{http_code}' "${base_url}/" || true)"
    if [ "$code" = "200" ]; then
      return 0
    fi
    sleep 10
  done

  echo "AWVS did not become ready in time"
  exit 1
}

build_protocol() {
  local name="$1"
  local url="$2"
  local api_key="$3"
  local manager_url="$4"
  local manager_token="$5"
  local username="$6"
  local password="$7"
  local max_concurrency="$8"
  local json

  json="$(printf '{"name":"%s","url":"%s","api_key":"%s","manager_url":"%s","manager_token":"%s","awvs_username":"%s","awvs_password":"%s","max_concurrency":%s}' "$name" "$url" "$api_key" "$manager_url" "$manager_token" "$username" "$password" "$max_concurrency")"
  printf 'awvsagent://%s\n' "$(printf '%s' "$json" | base64 | tr -d '\n')"
}

ensure_packages
ensure_docker

SAFE_NAME="$(sanitize_name "$AGENT_NAME")"
CONTAINER_NAME="awvs-agent-${SAFE_NAME}"
DATA_ROOT="/opt/aspanel/awvs-agent/${SAFE_NAME}"
MANAGER_TOKEN_FILE="${DATA_ROOT}/manager_token"
MANAGER_PORT_FILE="${DATA_ROOT}/manager_port"
MANAGER_CONFIG_FILE="${DATA_ROOT}/manager_config.json"
MANAGER_BIN_FILE="${DATA_ROOT}/docker-manager"
MANAGER_PID_FILE="${DATA_ROOT}/docker-manager.pid"
MANAGER_STATE_FILE="${DATA_ROOT}/docker-manager.state.json"
MANAGER_LOG_FILE="${DATA_ROOT}/docker-manager.log"
UPDATE_SCRIPT_FILE="${DATA_ROOT}/update-agent.sh"
UPDATE_LOG_FILE="${DATA_ROOT}/update-agent.log"
mkdir -p "$DATA_ROOT"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

backup_existing_awvs_state() {
  local backup_root="${WORKDIR}/awvs-state"
  rm -rf "$backup_root"
  mkdir -p "$backup_root"
  if ! $SUDO docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
    return 1
  fi
  echo "[*] Backing up existing AWVS state..."
  if ! $SUDO docker cp "${CONTAINER_NAME}:/home/acunetix/.acunetix" "$backup_root" >/dev/null 2>&1; then
    echo "[!] Failed to backup existing AWVS state."
    return 1
  fi
  return 0
}

restore_existing_awvs_state() {
  local backup_path="${WORKDIR}/awvs-state/.acunetix"
  if [ ! -d "$backup_path" ]; then
    return 1
  fi
  echo "[*] Restoring previous AWVS state..."
  $SUDO docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
  if ! $SUDO docker cp "$backup_path" "${CONTAINER_NAME}:/home/acunetix/" >/dev/null 2>&1; then
    echo "[!] Failed to restore previous AWVS state."
    return 1
  fi
  if ! $SUDO docker start "$CONTAINER_NAME" >/dev/null 2>&1; then
    echo "[!] Failed to restart AWVS after state restore."
    return 1
  fi
  return 0
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

PUBLIC_HOST="$(curl -fsSL https://api.ipify.org 2>/dev/null || true)"
if [ -z "$PUBLIC_HOST" ]; then
  PUBLIC_HOST="$(hostname -I | awk '{print $1}')"
fi
MANAGER_URL="http://${PUBLIC_HOST}:${MANAGER_PORT}"

# Reuse the original port for in-place updates of the same node.
RESTORE_PREVIOUS_STATE=0
if [ "${MANAGER_ALLOW_REUSE_PORT:-0}" = "1" ] && $SUDO docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
  if ! backup_existing_awvs_state; then
    exit 1
  fi
  RESTORE_PREVIOUS_STATE=1
  $SUDO docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
fi

# Re-allocate a random port only when the requested one is truly occupied.
while ! check_port_free "$AGENT_PORT"; do
  echo "[!] Port $AGENT_PORT is already in use. Assigning a new random port..."
  AGENT_PORT="$((30000 + RANDOM % 10001))"
done

$SUDO docker pull "$IMAGE" >/dev/null
$SUDO docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
$SUDO docker run -d \
  --name "$CONTAINER_NAME" \
  -p "${AGENT_PORT}:${AWVS_CONTAINER_PORT}" \
  --cap-add LINUX_IMMUTABLE \
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
  printf 'MANAGER_ALLOW_REUSE_PORT=1 curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/awvs-agent-entrypoint.sh | bash -s -- -n %q -p %q -c %q' "$AGENT_NAME" "$AGENT_PORT" "$MAX_CONCURRENT"
  echo
} | $SUDO tee "$UPDATE_SCRIPT_FILE" >/dev/null
$SUDO chmod +x "$UPDATE_SCRIPT_FILE"
if [ -f "$MANAGER_PID_FILE" ]; then
  OLD_PID="$(cat "$MANAGER_PID_FILE" 2>/dev/null || true)"
  if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" >/dev/null 2>&1; then
    kill "$OLD_PID" >/dev/null 2>&1 || true
  fi
fi
nohup "$MANAGER_BIN_FILE" --port "$MANAGER_PORT" --token "$MANAGER_TOKEN" --config "$MANAGER_CONFIG_FILE" --state-file "$MANAGER_STATE_FILE" >> "$MANAGER_LOG_FILE" 2>&1 &
echo $! > "$MANAGER_PID_FILE"

BASE_URL="https://${PUBLIC_HOST}:${AGENT_PORT}"
LOCAL_URL="https://127.0.0.1:${AGENT_PORT}"

if [ "$RESTORE_PREVIOUS_STATE" = "1" ]; then
  if ! restore_existing_awvs_state; then
    exit 1
  fi
  wait_for_awvs "$LOCAL_URL"
  echo ""
  echo "=========================================="
  echo "[+] AWVS Update Complete"
  echo "=========================================="
  echo "URL: ${BASE_URL}"
  echo "[+] Previous AWVS state restored."
  echo "[+] Existing username/password/api key should remain unchanged."
  exit 0
fi

wait_for_awvs "$LOCAL_URL"

SESSION_TOKEN="$(login_awvs "$LOCAL_URL" "$WORKDIR")"
if [ -z "$SESSION_TOKEN" ]; then
  echo "failed to obtain awvs session token"
  exit 1
fi

API_KEY=""

printf '{"query":"query { apiKey }"}' > "${WORKDIR}/query_api_key.json"
if graphql_request "$LOCAL_URL" "$SESSION_TOKEN" "$WORKDIR" "${WORKDIR}/query_api_key.json" "${WORKDIR}/api_key.json"; then
  API_KEY="$(extract_first_json_string_value "${WORKDIR}/api_key.json" "apiKey" "apikey" || true)"
fi

if [ -z "$API_KEY" ]; then
  printf '{"query":"query { apikey }"}' > "${WORKDIR}/query_apikey_alt.json"
  if graphql_request "$LOCAL_URL" "$SESSION_TOKEN" "$WORKDIR" "${WORKDIR}/query_apikey_alt.json" "${WORKDIR}/api_key_alt.json"; then
    API_KEY="$(extract_first_json_string_value "${WORKDIR}/api_key_alt.json" "apiKey" "apikey" || true)"
  fi
fi

if [ -z "$API_KEY" ]; then
  printf '{"query":"mutation { generateApiKey }"}' > "${WORKDIR}/generate_api_key.json"
  if graphql_request "$LOCAL_URL" "$SESSION_TOKEN" "$WORKDIR" "${WORKDIR}/generate_api_key.json" "${WORKDIR}/generated_api_key.json"; then
    API_KEY="$(extract_first_json_string_value "${WORKDIR}/generated_api_key.json" "generateApiKey" "apiKey" "apikey" || true)"
  fi
fi

if [ -z "$API_KEY" ]; then
  printf '{"query":"mutation { generateAPIKey }"}' > "${WORKDIR}/generate_api_key_upper.json"
  if graphql_request "$LOCAL_URL" "$SESSION_TOKEN" "$WORKDIR" "${WORKDIR}/generate_api_key_upper.json" "${WORKDIR}/generated_api_key_upper.json"; then
    API_KEY="$(extract_first_json_string_value "${WORKDIR}/generated_api_key_upper.json" "generateAPIKey" "apiKey" "apikey" || true)"
  fi
fi

if [ -z "$API_KEY" ]; then
  API_KEY="$(get_api_key_from_rest "$LOCAL_URL" "$SESSION_TOKEN" "$WORKDIR" || true)"
fi

if [ -z "$API_KEY" ]; then
  echo "[!] Failed to obtain dedicated API key, fallback to session token."
  API_KEY="$SESSION_TOKEN"
fi

if [ -z "$API_KEY" ]; then
  echo "failed to obtain awvs api key"
  exit 1
fi

echo "[*] Changing default password..."
NEW_AWVS_PASSWORD="Awvs1@$(head /dev/urandom | tr -dc A-Za-z0-9 | head -c 12)"
OLD_HASH="$(printf '%s' "$AWVS_PASSWORD" | sha256sum | awk '{print $1}')"
NEW_HASH="$(printf '%s' "$NEW_AWVS_PASSWORD" | sha256sum | awk '{print $1}')"

printf '{"operationName":"updatePassword","variables":{"credentials":{"email":"%s","currentPassword":"%s","newPassword":"%s"}},"query":"mutation updatePassword($credentials: ChangeCredentialsInput!) {\\n  updatePassword(credentials: $credentials)\\n}"}' "$AWVS_EMAIL" "$OLD_HASH" "$NEW_HASH" > "${WORKDIR}/update_password.json"

if graphql_request "$LOCAL_URL" "$SESSION_TOKEN" "$WORKDIR" "${WORKDIR}/update_password.json" "${WORKDIR}/update_password_response.json"; then
  AWVS_PASSWORD="$NEW_AWVS_PASSWORD"
  echo "[+] Password changed successfully."
else
  echo "[-] Failed to change password, keeping default."
fi

echo ""
echo "=========================================="
echo "[+] AWVS Installation Complete"
echo "=========================================="
echo "URL: ${BASE_URL}"
echo "Username: ${AWVS_EMAIL}"
echo "Password: ${AWVS_PASSWORD}"
echo ""
build_protocol "$AGENT_NAME" "$BASE_URL" "$API_KEY" "$MANAGER_URL" "$MANAGER_TOKEN" "$AWVS_EMAIL" "$AWVS_PASSWORD" "$MAX_CONCURRENT"
