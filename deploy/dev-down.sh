#!/usr/bin/env bash
# Stop the MCPDoll local stack.
#
# Stops exactly the processes dev-up.sh started, by recorded PID. Killing by name
# would take out an unrelated mcpdoll-dp a colleague happens to be running.
set -uo pipefail

cd "$(dirname "$0")/.."

LOCAL_DIR=deploy/local
PID_FILE="${LOCAL_DIR}/dev.pids"

info() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }

compose() {
  if docker compose version >/dev/null 2>&1; then
    docker compose -f deploy/docker-compose.yml "$@"
  elif command -v docker-compose >/dev/null 2>&1; then
    docker-compose -f deploy/docker-compose.yml "$@"
  else
    return 1
  fi
}

if [[ -f "${PID_FILE}" ]]; then
  info "stopping local processes"
  while read -r pid; do
    [[ -z "${pid}" ]] && continue
    if kill -0 "${pid}" 2>/dev/null; then
      # SIGTERM, not SIGKILL: the data plane drains in-flight calls and flushes
      # telemetry on a clean shutdown, which is exactly the data you want.
      kill "${pid}" 2>/dev/null || true
    fi
  done < "${PID_FILE}"

  # Give them a moment to exit cleanly, then insist.
  sleep 2
  while read -r pid; do
    [[ -z "${pid}" ]] && continue
    if kill -0 "${pid}" 2>/dev/null; then
      info "process ${pid} did not exit; sending SIGKILL"
      kill -9 "${pid}" 2>/dev/null || true
    fi
  done < "${PID_FILE}"

  rm -f "${PID_FILE}"
else
  info "no recorded processes (already stopped?)"
fi

if compose version >/dev/null 2>&1; then
  info "stopping the LGTM stack and deleting its volumes"
  compose down -v 2>/dev/null || true
fi

info "done"
