#!/usr/bin/env bash
# Build and start the containerized stack, and return only once it is usable.
#
# "Usable" is not "containers started". Compose reports a container up the
# moment its process execs, and the data plane spends a second or two loading
# WASM plugins before it can serve. Returning early is how a script that runs
# right after this one fails for no reason anybody can see.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE=(docker compose -f deploy/docker-compose.yml)
TOKEN="${MCPDOLL_CP_TOKEN:-dev-token-not-a-secret}"

info()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33mwarn\033[0m %s\n' "$*" >&2; }
die()   { printf '\033[1;31merror\033[0m %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker is not on PATH"
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable — is Docker running?"

# Refuse to start on top of the host stack. Both bind the same ports, so the
# containers would fail to publish and the error ("port already allocated")
# says nothing about the actual cause.
if [[ -f deploy/local/dev.pids ]] && command -v lsof >/dev/null 2>&1; then
  if lsof -nP -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1; then
    if ! docker ps --format '{{.Names}}' | grep -q '^mcpdoll-dp$'; then
      die "port 8080 is held by the host stack — run 'make dev-down' first"
    fi
  fi
fi

info "building images"
"${COMPOSE[@]}" build

info "starting the stack"
# --wait blocks on every healthcheck, so this returns when the stack is
# actually serving rather than when the last container was created.
if ! "${COMPOSE[@]}" up -d --wait --wait-timeout 240; then
  warn "the stack did not become healthy; recent logs follow"
  "${COMPOSE[@]}" ps
  "${COMPOSE[@]}" logs --tail=40
  die "startup failed"
fi

# Prove it end to end rather than trusting the healthchecks, which only ask
# whether each container answers about itself. This asks whether the pieces can
# see each other.
info "verifying the stack answers"
snapshot="$(curl -fsS http://localhost:8080/readyz | sed 's/.*"snapshot_version":\([0-9]*\).*/\1/')"
backends="$(curl -fsS http://localhost:8081/admin/backends \
  | sed 's/.*"healthy":\([0-9]*\).*/\1/' | head -1)"
curl -fsS -H "Authorization: Bearer ${TOKEN}" \
  http://localhost:3001/api/v1/registry >/dev/null

cat <<BANNER

MCPDoll is up, in Docker.

  Console         http://localhost:5173
  Control plane   http://localhost:3001   (token: ${TOKEN})
  Data plane      http://localhost:8080
  Admin           http://localhost:8081/admin/backends
  Grafana         http://localhost:3300   (folder: MCPDoll)

  Serving snapshot ${snapshot} with ${backends} healthy backend(s).

Try it:

  # What a support agent sees, asked as a real MCP client
  ./bin/mcpdoll gateway catalog --audience support-agents --subject alice@example.com

  # The redact plugin at work: the backend returns a card number, the model does not
  ./bin/mcpdoll gateway call crm.get_payment_method --audience support-agents \\
      --args '{"customer_id":"cus_1"}'

  # What the prober knows. Exits 5 if drift is blocking anything.
  ./bin/mcpdoll gateway backends

  make ps        what is running        make logs SERVICE=dataplane
  make down      stop, keep the key     make down-hard   stop and wipe

BANNER
