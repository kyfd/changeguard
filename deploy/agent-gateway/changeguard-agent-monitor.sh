#!/usr/bin/env bash
set -u -o pipefail

base_url="${CHANGEGUARD_AGENT_MONITOR_BASE_URL:-http://127.0.0.1:18081}"
state_file="${CHANGEGUARD_AGENT_MONITOR_STATE_FILE:-/opt/changeguard-agent/data/monitor.json}"
webhook_url="${CHANGEGUARD_AGENT_ALERT_WEBHOOK_URL:-}"
webhook_token="${CHANGEGUARD_AGENT_ALERT_WEBHOOK_TOKEN:-}"
checked_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
host_name="$(hostname 2>/dev/null || printf unknown)"
state_directory="$(dirname "$state_file")"

mkdir -p "$state_directory" || exit 1
previous_status="unknown"
if [ -r "$state_file" ]; then
  if grep -q '"status":"degraded"' "$state_file"; then
    previous_status="degraded"
  elif grep -q '"status":"ok"' "$state_file"; then
    previous_status="ok"
  fi
fi

temporary_state="$(mktemp "$state_directory/.monitor.json.tmp.XXXXXX")" || exit 1
temporary_curl=""
cleanup() {
	if [ -n "$temporary_state" ]; then
	  rm -f "$temporary_state"
	fi
  if [ -n "$temporary_curl" ]; then
    rm -f "$temporary_curl"
  fi
}
trap cleanup EXIT

json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  printf '%s' "$value"
}

curl_config_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/}"
  printf '%s' "$value"
}

send_webhook() {
  local status="$1"
  local reason="$2"
  if [ -z "$webhook_url" ]; then
    return 0
  fi
  local payload
  payload="$(printf '{"event":"changeguard_agent_gateway_health","status":"%s","reason":"%s","checked_at":"%s","host":"%s"}' \
    "$(json_escape "$status")" "$(json_escape "$reason")" "$(json_escape "$checked_at")" "$(json_escape "$host_name")")"
  temporary_curl="$(mktemp)" || return 1
  chmod 0600 "$temporary_curl" || return 1
  {
    printf 'url = "%s"\n' "$(curl_config_escape "$webhook_url")"
    printf 'request = "POST"\n'
    printf 'header = "Content-Type: application/json"\n'
    if [ -n "$webhook_token" ]; then
      printf 'header = "Authorization: Bearer %s"\n' "$(curl_config_escape "$webhook_token")"
    fi
    printf 'data = "%s"\n' "$(curl_config_escape "$payload")"
    printf 'silent\nshow-error\nfail\nmax-time = 5\n'
  } > "$temporary_curl"
  curl --config "$temporary_curl" >/dev/null
}

write_state() {
  local status="$1"
  local reason="$2"
  printf '{"schema":"changeguard-agent-monitor/v1","checked_at":"%s","status":"%s","reason":"%s"}\n' \
    "$(json_escape "$checked_at")" "$(json_escape "$status")" "$(json_escape "$reason")" > "$temporary_state" || return 1
  chmod 0640 "$temporary_state" || return 1
  mv -f "$temporary_state" "$state_file"
  temporary_state=""
}

failure_reason=""
if ! curl -fsS --max-time 5 "$base_url/health/ready" >/dev/null; then
  failure_reason="readiness_failed"
elif ! curl -fsS --max-time 5 "$base_url/health/slo" >/dev/null; then
  failure_reason="slo_degraded"
fi

if [ -n "$failure_reason" ]; then
  write_state "degraded" "$failure_reason" || exit 1
  if [ "$previous_status" != "degraded" ]; then
    send_webhook "degraded" "$failure_reason" || printf 'webhook_delivery=failed status=degraded reason=%s\n' "$failure_reason" >&2
  fi
  printf 'status=degraded reason=%s checked_at=%s\n' "$failure_reason" "$checked_at" >&2
  exit 1
fi

write_state "ok" "healthy" || exit 1
if [ "$previous_status" = "degraded" ]; then
  send_webhook "recovered" "healthy" || printf 'webhook_delivery=failed status=recovered\n' >&2
fi
printf 'status=ok checked_at=%s\n' "$checked_at"
