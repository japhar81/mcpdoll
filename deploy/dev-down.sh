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
      # Children first. `npm run dev` is a wrapper around vite, and killing the
      # wrapper orphans the server that is actually holding :5173 — so the next
      # `make dev` refuses to start on a port nothing appears to own.
      pkill -TERM -P "${pid}" 2>/dev/null || true
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
      pkill -KILL -P "${pid}" 2>/dev/null || true
      kill -9 "${pid}" 2>/dev/null || true
    fi
  done < "${PID_FILE}"

  rm -f "${PID_FILE}"

  # A last check on the ports themselves. Killing by pid is right and is not
  # sufficient: anything the recorded processes spawned indirectly survives it,
  # and the symptom is a `make dev` that refuses to start against a port whose
  # owner nobody can name.
  for port in 8080 8081 3001 5173 9101 9102 9103 9104 9105 9106; do
    stray="$(lsof -ti ":${port}" -sTCP:LISTEN 2>/dev/null || true)"
    if [[ -n "${stray}" ]]; then
      info "port ${port} is still held by ${stray}; stopping it"
      kill -9 ${stray} 2>/dev/null || true
    fi
  done
else
  info "no recorded processes (already stopped?)"
fi

if compose version >/dev/null 2>&1; then
  # Stop, do not destroy. `down -v` was right when the only container was LGTM
  # and its volume held a few minutes of dev telemetry. It is wrong now that
  # the same compose file owns Postgres: that volume holds every tenant, user,
  # grant, and API key, and wiping it on `make dev-down` would mean a stack
  # that forgets who exists every time it is stopped.
  info "stopping the containers (Postgres data is kept)"
  compose stop 2>/dev/null || true
fi

info "done"

cat <<'NOTE'

Containers are stopped, not removed, and Postgres keeps its data — tenants,
users, and grants survive. To wipe it and start from an empty database:

  docker compose -f deploy/docker-compose.yml down -v

That also discards the demo credentials, which cannot be recovered. The next
`make dev` mints new ones.
NOTE
