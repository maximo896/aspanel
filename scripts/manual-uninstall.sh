#!/bin/bash
set -euo pipefail

CONTAINERS=""
SERVICE_NAME=""
DATA_ROOT=""
CLEAR_AWVS_IMMUTABLE=0

while getopts "c:s:d:i" opt; do
  case "$opt" in
    c) CONTAINERS="$OPTARG" ;;
    s) SERVICE_NAME="$OPTARG" ;;
    d) DATA_ROOT="$OPTARG" ;;
    i) CLEAR_AWVS_IMMUTABLE=1 ;;
  esac
done

if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
else
  SUDO="sudo"
fi

clear_awvs_immutable() {
  local cn="$1"
  if ! $SUDO docker inspect "$cn" >/dev/null 2>&1; then
    return 0
  fi
  $SUDO docker start "$cn" >/dev/null 2>&1 || true
  $SUDO docker exec -u 0 "$cn" sh -c '
    for p in /home/acunetix /home/acunetix/.acunetix /opt/acunetix /var/lib/acunetix /var/opt/acunetix; do
      if [ -e "$p" ] && command -v chattr >/dev/null 2>&1; then
        chattr -R -i -a "$p" >/dev/null 2>&1 || true
      fi
    done
  ' >/dev/null 2>&1 || true
  if command -v chattr >/dev/null 2>&1; then
    $SUDO docker inspect -f '{{range $k,$v := .GraphDriver.Data}}{{println $v}}{{end}}' "$cn" 2>/dev/null | while IFS= read -r p; do
      case "$p" in
        /var/lib/docker/*) [ -e "$p" ] && $SUDO chattr -R -i -a "$p" >/dev/null 2>&1 || true ;;
      esac
    done
  fi
}

IFS=',' read -r -a CONTAINER_ARRAY <<< "$CONTAINERS"
for cn in "${CONTAINER_ARRAY[@]}"; do
  cn="$(printf '%s' "$cn" | xargs)"
  if [ -z "$cn" ]; then
    continue
  fi
  if [ "$CLEAR_AWVS_IMMUTABLE" = "1" ]; then
    clear_awvs_immutable "$cn"
    $SUDO docker rm -f "$cn" >/dev/null 2>&1 || { clear_awvs_immutable "$cn"; $SUDO docker rm -f "$cn" >/dev/null 2>&1 || true; }
  else
    $SUDO docker rm -f "$cn" >/dev/null 2>&1 || true
  fi
done

if [ -n "$DATA_ROOT" ] && [ "$DATA_ROOT" != "." ] && [ "$DATA_ROOT" != "/" ]; then
  if [ "$CLEAR_AWVS_IMMUTABLE" = "1" ] && command -v chattr >/dev/null 2>&1 && [ -d "$DATA_ROOT" ]; then
    $SUDO chattr -R -i -a "$DATA_ROOT" >/dev/null 2>&1 || true
  fi
  $SUDO rm -rf "$DATA_ROOT"
fi

if [ -n "$SERVICE_NAME" ] && command -v systemctl >/dev/null 2>&1; then
  $SUDO systemctl disable "$SERVICE_NAME" >/dev/null 2>&1 || true
  $SUDO systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
  $SUDO rm -f "/etc/systemd/system/${SERVICE_NAME}.service"
  $SUDO systemctl daemon-reload >/dev/null 2>&1 || true
fi

echo "[+] Uninstall complete"
