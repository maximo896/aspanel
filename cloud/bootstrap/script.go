package bootstrap

import "fmt"

type ScriptOptions struct {
	AWVSInstallCommand   string
	SQLMapInstallCommand string
	PathInstallCommand   string
	CallbackURL          string
	Token                string
	Region               string
	Zone                 string
}

func BuildInitScript(opts ScriptOptions) string {
	return fmt.Sprintf(`#!/bin/bash
set -euo pipefail

AWVS_INSTALL_CMD=%q
SQLMAP_INSTALL_CMD=%q
PATH_INSTALL_CMD=%q

AWVS_LOG=/tmp/awvs-install.log
SQLMAP_LOG=/tmp/sqlmap-install.log
PATH_LOG=/tmp/path-install.log
RUN_LOG=/tmp/panel-bootstrap.log

touch "$RUN_LOG"
{
  echo "[panel-bootstrap] started at $(date -u +"%%Y-%%m-%%dT%%H:%%M:%%SZ")"
  echo "[panel-bootstrap] awvs_log=$AWVS_LOG"
  echo "[panel-bootstrap] sqlmap_log=$SQLMAP_LOG"
  echo "[panel-bootstrap] path_log=$PATH_LOG"
} >> "$RUN_LOG"

CALLBACK_URL=%q
TOKEN=%q
REGION=%q
ZONE=%q

if [ -n "$CALLBACK_URL" ] && [ -n "$TOKEN" ]; then
  curl -fsSL --max-time 10 -G \
    --data-urlencode "token=$TOKEN" \
    --data-urlencode "kind=bootstrap" \
    --data-urlencode "region=$REGION" \
    --data-urlencode "zone=$ZONE" \
    --data-urlencode "proto=bootstrap://installing" \
    "$CALLBACK_URL" >/dev/null 2>&1 || true
fi

bash -lc "$AWVS_INSTALL_CMD" 2>&1 | tee "$AWVS_LOG" >> "$RUN_LOG" || true
bash -lc "$SQLMAP_INSTALL_CMD" 2>&1 | tee "$SQLMAP_LOG" >> "$RUN_LOG" || true
bash -lc "$PATH_INSTALL_CMD" 2>&1 | tee "$PATH_LOG" >> "$RUN_LOG" || true

AWVS_LINK=$(grep -Eo "awvsagent://[^[:space:]]*" "$AWVS_LOG" 2>/dev/null | tail -n 1 || true)
SQLMAP_LINK=$(grep -Eo "sqlmapagent://[^[:space:]]*" "$SQLMAP_LOG" 2>/dev/null | tail -n 1 || true)
PATH_LINK=$(grep -Eo "pathagent://[^[:space:]]*" "$PATH_LOG" 2>/dev/null | tail -n 1 || true)

{
  echo "[panel-bootstrap] callback_url=$CALLBACK_URL"
  echo "[panel-bootstrap] token=$TOKEN region=$REGION zone=$ZONE"
  echo "[panel-bootstrap] awvs_link_found=$([ -n "$AWVS_LINK" ] && echo yes || echo no)"
  echo "[panel-bootstrap] sqlmap_link_found=$([ -n "$SQLMAP_LINK" ] && echo yes || echo no)"
  echo "[panel-bootstrap] path_link_found=$([ -n "$PATH_LINK" ] && echo yes || echo no)"
} >> "$RUN_LOG"

if [ -n "$AWVS_LINK" ]; then
  if [ -n "$CALLBACK_URL" ] && [ -n "$TOKEN" ]; then
    curl -fsSL --max-time 10 -G \
      --data-urlencode "token=$TOKEN" \
      --data-urlencode "kind=awvs" \
      --data-urlencode "region=$REGION" \
      --data-urlencode "zone=$ZONE" \
      --data-urlencode "proto=$AWVS_LINK" \
      "$CALLBACK_URL" >/dev/null 2>&1 || true
  fi
fi
if [ -n "$SQLMAP_LINK" ]; then
  if [ -n "$CALLBACK_URL" ] && [ -n "$TOKEN" ]; then
    curl -fsSL --max-time 10 -G \
      --data-urlencode "token=$TOKEN" \
      --data-urlencode "kind=sqlmap" \
      --data-urlencode "region=$REGION" \
      --data-urlencode "zone=$ZONE" \
      --data-urlencode "proto=$SQLMAP_LINK" \
      "$CALLBACK_URL" >/dev/null 2>&1 || true
  fi
fi
if [ -n "$PATH_LINK" ]; then
  if [ -n "$CALLBACK_URL" ] && [ -n "$TOKEN" ]; then
    curl -fsSL --max-time 10 -G \
      --data-urlencode "token=$TOKEN" \
      --data-urlencode "kind=path" \
      --data-urlencode "region=$REGION" \
      --data-urlencode "zone=$ZONE" \
      --data-urlencode "proto=$PATH_LINK" \
      "$CALLBACK_URL" >/dev/null 2>&1 || true
  fi
fi

HEARTBEAT_INTERVAL_SEC=60
if [ -n "$CALLBACK_URL" ] && [ -n "$TOKEN" ] && { [ -n "$AWVS_LINK" ] || [ -n "$SQLMAP_LINK" ] || [ -n "$PATH_LINK" ]; }; then
  {
    echo "[panel-bootstrap] starting resident heartbeat loop interval=${HEARTBEAT_INTERVAL_SEC}s"
  } >> "$RUN_LOG"
  nohup bash -c '
    CALLBACK_URL="$1"
    TOKEN="$2"
    REGION="$3"
    ZONE="$4"
    AWVS_LINK="$5"
    SQLMAP_LINK="$6"
    PATH_LINK="$7"
    HEARTBEAT_INTERVAL_SEC="$8"
    while true; do
      if [ -n "$AWVS_LINK" ]; then
        curl -fsSL --max-time 10 -G \
          --data-urlencode "token=$TOKEN" \
          --data-urlencode "kind=awvs" \
          --data-urlencode "region=$REGION" \
          --data-urlencode "zone=$ZONE" \
          --data-urlencode "proto=$AWVS_LINK" \
          "$CALLBACK_URL" >/dev/null 2>&1 || true
      fi
      if [ -n "$SQLMAP_LINK" ]; then
        curl -fsSL --max-time 10 -G \
          --data-urlencode "token=$TOKEN" \
          --data-urlencode "kind=sqlmap" \
          --data-urlencode "region=$REGION" \
          --data-urlencode "zone=$ZONE" \
          --data-urlencode "proto=$SQLMAP_LINK" \
          "$CALLBACK_URL" >/dev/null 2>&1 || true
      fi
      if [ -n "$PATH_LINK" ]; then
        curl -fsSL --max-time 10 -G \
          --data-urlencode "token=$TOKEN" \
          --data-urlencode "kind=path" \
          --data-urlencode "region=$REGION" \
          --data-urlencode "zone=$ZONE" \
          --data-urlencode "proto=$PATH_LINK" \
          "$CALLBACK_URL" >/dev/null 2>&1 || true
      fi
      sleep "$HEARTBEAT_INTERVAL_SEC"
    done
  ' _ "$CALLBACK_URL" "$TOKEN" "$REGION" "$ZONE" "$AWVS_LINK" "$SQLMAP_LINK" "$PATH_LINK" "$HEARTBEAT_INTERVAL_SEC" >> /tmp/panel-heartbeat.log 2>&1 &
fi

echo "[panel-bootstrap] finished at $(date -u +"%%Y-%%m-%%dT%%H:%%M:%%SZ")" >> "$RUN_LOG"
`, opts.AWVSInstallCommand, opts.SQLMapInstallCommand, opts.PathInstallCommand, opts.CallbackURL, opts.Token, opts.Region, opts.Zone)
}
