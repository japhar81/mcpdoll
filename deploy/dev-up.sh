#!/usr/bin/env bash
# Bring up the MCPDoll local stack.
#
# What this starts, in order:
#   1. The LGTM observability stack (Grafana on :3300) and Postgres, via Docker.
#   2. Six fixture MCP backends on :9101-:9106.
#   3. The control plane on :3001, which migrates the database and seeds the
#      first administrator.
#   4. Demo tenants, users, grants, and API keys, through the CLI.
#   5. A signed snapshot built by discovering those backends and carrying that
#      tenancy.
#   6. The data plane on :8080, serving from that snapshot.
#
# The control plane comes *before* the snapshot because the snapshot carries
# tenants and principals, and those live in the database. Building first is the
# cycle this ordering exists to avoid.
#
# Everything except the containers runs as a local process, so `make dev` works
# without building images and a code change is one `make dev` away from being
# live. Logs land in deploy/local/logs/.
set -euo pipefail

cd "$(dirname "$0")/.."

LOCAL_DIR=deploy/local
LOG_DIR="${LOCAL_DIR}/logs"
PID_FILE="${LOCAL_DIR}/dev.pids"
KEY_ID=dev

mkdir -p "${LOG_DIR}"

# ---------------------------------------------------------------- helpers ----

info()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m warn\033[0m %s\n' "$*"; }
die()   { printf '\033[1;31merror\033[0m %s\n' "$*" >&2; exit 1; }

# Track every process we start so dev-down.sh can stop exactly those and nothing
# else. Killing by name would take out a colleague's unrelated `mcpdoll-dp`.
record_pid() { echo "$1" >> "${PID_FILE}"; }

# wait_http polls a URL until it answers or the budget runs out. Started
# processes are checked for liveness too, so a crash reports the crash rather
# than a timeout.
wait_http() {
  local url=$1 name=$2 pid=${3:-} attempts=${4:-60}
  for _ in $(seq 1 "${attempts}"); do
    if curl -sf -o /dev/null "${url}"; then
      return 0
    fi
    if [[ -n "${pid}" ]] && ! kill -0 "${pid}" 2>/dev/null; then
      echo
      warn "${name} exited during startup; last 20 lines of its log:"
      tail -20 "${LOG_DIR}/${name}.log" >&2 || true
      return 1
    fi
    printf '.'
    sleep 0.5
  done
  echo
  warn "${name} did not become ready at ${url}"
  return 1
}

compose() {
  if docker compose version >/dev/null 2>&1; then
    docker compose -f deploy/docker-compose.yml "$@"
  elif command -v docker-compose >/dev/null 2>&1; then
    docker-compose -f deploy/docker-compose.yml "$@"
  else
    return 1
  fi
}

# ------------------------------------------------------------- preflight ----

command -v go >/dev/null || die "go is not on PATH"

# Refuse to start on top of an existing stack.
#
# Without this the script "succeeds" against whatever is already listening: the
# health checks pass, the banner prints, and the snapshot it just built is
# quietly refused by the older process as a stale version. The result is a
# developer debugging a gateway that is not the one they think they started.
check_port_free() {
  local port=$1 what=$2
  if lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1; then
    die "port ${port} (${what}) is already in use — run 'make dev-down' first, "\
        "or stop whatever is listening there"
  fi
}

if command -v lsof >/dev/null 2>&1; then
  check_port_free 8080 "data plane"
  check_port_free 3001 "control plane"
  check_port_free 5173 "console"
  for port in 9101 9102 9103 9104 9105; do
    check_port_free "${port}" "fixture backend"
  done
else
  warn "lsof is unavailable; skipping the port-conflict check"
fi

# ------------------------------------------------------------------ build ----

info "building binaries"
mkdir -p bin
go build -o bin/mcpdoll ./cmd/mcpdoll
go build -o bin/mcpdoll-dp ./cmd/mcpdoll-dp
go build -o bin/mcpdoll-cp ./cmd/mcpdoll-cp
go build -o bin/fixture-backend ./fixtures/cmd/fixture-backend

# ------------------------------------------------------- observability -------

if compose version >/dev/null 2>&1; then
  info "starting the LGTM observability stack"
  if ! compose up -d otel-collector; then
    warn "could not start the LGTM stack; continuing without it"
    warn "traces and metrics will be created but not exported"
  else
    # Grafana takes a while; do not block the rest of startup on it.
    ( wait_http http://localhost:3300/api/health lgtm "" 120 >/dev/null 2>&1 \
        && info "Grafana ready at http://localhost:3300" ) &
  fi
else
  warn "docker compose is unavailable; skipping the LGTM stack"
  warn "traces and metrics will be created but not exported"
fi

# ------------------------------------------------------------- postgres -----

# No longer optional. Tenants, users, grants, and API keys live here, and a
# snapshot that carries none of them serves nobody — so a stack without a
# database is not a smaller stack, it is a broken one.
if compose version >/dev/null 2>&1; then
  info "starting Postgres"
  compose up -d postgres >/dev/null \
    || die "could not start Postgres; the control plane cannot run without it"
  for _ in $(seq 1 60); do
    if compose exec -T postgres pg_isready -U mcpdoll -d mcpdoll >/dev/null 2>&1; then
      break
    fi
    printf '.'
    sleep 0.5
  done
  echo
else
  die "docker compose is unavailable, and Postgres is required: tenants, users, and grants live there"
fi

# --------------------------------------------------------------- fixtures ----

: > "${PID_FILE}"

start_fixture() {
  local kind=$1 port=$2
  shift 2
  info "starting fixture ${kind} on :${port}"
  ./bin/fixture-backend -kind "${kind}" -addr ":${port}" "$@" \
    > "${LOG_DIR}/fixture-${kind}.log" 2>&1 &
  local pid=$!
  record_pid "${pid}"
  wait_http "http://localhost:${port}/healthz" "fixture-${kind}" "${pid}" \
    || die "fixture ${kind} failed to start"
  echo
}

start_fixture modern      9101
# Globex's own CRM deployment. A second container rather than a second binding
# to the first: pointing two tenants at one host would render correctly and
# demonstrate nothing, since the claim of ADR 0017 is that the data behind a
# toolset differs per tenant.
start_fixture modern      9106
start_fixture legacy      9102
# The warehouse fixture is deliberately a little slow and fails one call in
# seven, so the health board and the circuit breaker have something real to show.
start_fixture misbehaving 9103 -latency 120ms -fail-every 7
start_fixture hostile     9104
start_fixture confirming  9105

# ------------------------------------------------------------------- keys ----

if [[ ! -f "${LOCAL_DIR}/${KEY_ID}.key" ]]; then
  info "generating a local snapshot signing keypair"
  ./bin/mcpdoll keys generate --dir "${LOCAL_DIR}" --key-id "${KEY_ID}" --quiet >/dev/null
fi
TRUST_ENTRY="${KEY_ID}:$(tr -d '\n' < "${LOCAL_DIR}/${KEY_ID}.pub")"

# ---------------------------------------------------------------- plugins ----

# Build the first-party WASM plugins and rewrite their digests in the registry.
#
# The digest is what makes a swapped artifact fail closed, so it cannot be a
# constant in a committed file — it changes whenever a plugin does. Stamping it
# here means editing a plugin and re-running `make dev` just works, rather than
# failing with a mismatch that looks like a security incident.
info "building WASM plugins"
mkdir -p "${LOCAL_DIR}/plugins"
for plugin in redact entitlements; do
  GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
    -o "${LOCAL_DIR}/plugins/${plugin}.wasm" "./plugins/${plugin}"

  digest="sha256:$(shasum -a 256 "${LOCAL_DIR}/plugins/${plugin}.wasm" | cut -d' ' -f1)"
  # Replace the digest on the line following this plugin's artifact_ref.
  python3 - "${LOCAL_DIR}/registry.yaml" "${plugin}" "${digest}" <<'PYEOF'
import sys
path, plugin, digest = sys.argv[1], sys.argv[2], sys.argv[3]
lines = open(path).read().split("\n")
marker = f"artifact_ref: file://deploy/local/plugins/{plugin}.wasm"
for i, line in enumerate(lines):
    if marker in line and i + 1 < len(lines) and "artifact_digest:" in lines[i + 1]:
        indent = lines[i + 1][: len(lines[i + 1]) - len(lines[i + 1].lstrip())]
        lines[i + 1] = f'{indent}artifact_digest: "{digest}"'
        break
open(path, "w").write("\n".join(lines))
PYEOF
done

# ---------------------------------------------------------- control plane ----

export MCPDOLL_DATABASE_URL="${MCPDOLL_DATABASE_URL:-postgres://mcpdoll:mcpdoll@localhost:5432/mcpdoll?sslmode=disable}"

info "starting the control plane on :3001"
# A fixed development token rather than --allow-anonymous. Anonymous mode exists
# and works, but running the dev stack the way production runs means the console
# exercises the auth path every day rather than discovering it at deploy time.
export MCPDOLL_CP_TOKEN="${MCPDOLL_CP_TOKEN:-dev-token-not-a-secret}"
export MCPDOLL_TOKEN="${MCPDOLL_CP_TOKEN}"
./bin/mcpdoll-cp -config "${LOCAL_DIR}/dataplane.yaml" \
  > "${LOG_DIR}/mcpdoll-cp.log" 2>&1 &
CP_PID=$!
record_pid "${CP_PID}"
wait_http http://localhost:3001/healthz mcpdoll-cp "${CP_PID}" \
  || die "the control plane failed to start"
echo

# ------------------------------------------------------------------ seed ----

# Through the CLI against the API, not the database. The same path an operator
# uses, so a broken command fails here rather than the first time somebody types
# it.
info "seeding demo tenancy"
KEYS="${LOCAL_DIR}/demo-keys.txt"

have_tenant() {
  # A tenant the registry merely *binds* is listed too, with status
  # "unregistered" and no id. Matching on the slug alone would see that row and
  # skip creating the record — leaving a tenant nothing can authenticate into.
  ./bin/mcpdoll tenants list --output json 2>/dev/null | tr -d ' \n' \
    | grep -q "\"slug\":\"$1\",\"name\":[^}]*\"status\":\"active\""
}

have_user() {
  ./bin/mcpdoll users list --tenant "$1" --output json 2>/dev/null \
    | grep -q "\"email\": \"$2\""
}

for tenant in acme globex; do
  have_tenant "${tenant}" \
    || ./bin/mcpdoll tenants create "${tenant}" \
         --name "$(echo "${tenant}" | tr "[:lower:]" "[:upper:]")" --quiet >/dev/null
done

# Users and keys are separate concerns. A user survives across runs — they are
# rows in a database this script does not own — but the *keys file* is an
# artifact of this run, and a key's secret cannot be recovered once it is gone.
# So: create users that are missing, and mint a key whenever the file is absent,
# whether or not the user was new. A user may hold several keys, and the
# alternative is a banner pointing at a file that does not exist.
DEMO_USERS=(
  "acme support@acme.example|Support Agent|tool_user@t/acme/ts/support"
  "acme platform@acme.example|Platform Operator|tool_user@t/acme/ts/support,tool_user@t/acme/ts/platform"
  "acme research@acme.example|Threat Researcher|tool_user@t/acme/ts/untrusted"
  "globex support@globex.example|Support Agent|tool_user@t/globex/ts/support"
)

for entry in "${DEMO_USERS[@]}"; do
  who="${entry%%|*}"
  rest="${entry#*|}"
  label="${rest%%|*}"
  grants="${rest##*|}"
  tenant="${who%% *}"
  email="${who##* }"

  if ! have_user "${tenant}" "${email}"; then
    ./bin/mcpdoll users create "${email}" --tenant "${tenant}" --name "${label}" \
      --password demo-password-not-a-secret --quiet >/dev/null
    # Grants come second and separately, because a new user holding nothing is
    # the correct starting state.
    grant_args=()
    IFS=',' read -ra parts <<< "${grants}"
    for g in "${parts[@]}"; do grant_args+=(--grant "${g}"); done
    ./bin/mcpdoll users grants set "${email}" --tenant "${tenant}" \
      "${grant_args[@]}" --quiet >/dev/null
  fi
done

# Somebody who can actually use the console. `SeedPlatformAdmin` creates one
# with a generated password printed once, which is right for production and
# useless here — so this seeds a second with a password the README can name.
if ! have_user acme dev-admin@mcpdoll.local; then
  ./bin/mcpdoll users create dev-admin@mcpdoll.local --tenant acme \
    --name "Console Admin" --password demo-password-not-a-secret --quiet >/dev/null
  ./bin/mcpdoll users grants set dev-admin@mcpdoll.local --tenant acme \
    --grant "platform_admin@*" --quiet >/dev/null
  info "console admin: dev-admin@mcpdoll.local in tenant acme"
fi

if [[ ! -s "${KEYS}" ]]; then
  {
    echo "# MCPDoll demo credentials, minted $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    echo "# Every secret here is shown once and nothing keeps it. A previous"
    echo "# run's keys are not recoverable, so this run minted new ones."
    echo
  } > "${KEYS}"
  for entry in "${DEMO_USERS[@]}"; do
    who="${entry%%|*}"
    tenant="${who%% *}"
    email="${who##* }"
    secret="$(./bin/mcpdoll users keys mint "${email}" --tenant "${tenant}" \
      --name "dev-$(date +%s)" --output json --quiet \
      | sed -n 's/.*"secret": "\([^"]*\)".*/\1/p')"
    [[ -n "${secret}" ]] || die "minting a key for ${email} produced no secret"
    printf '%-30s %s\n' "${tenant}/${email}" "${secret}" >> "${KEYS}"
  done
  info "credentials written to ${KEYS}"
else
  info "reusing the credentials in ${KEYS}"
fi
echo

# --------------------------------------------------------------- snapshot ----

# After the seeding, because the snapshot carries the tenants and the API key
# digests those lines just created. The version is assigned by the build — a
# Unix timestamp, monotonic without anybody coordinating.
info "building a signed snapshot from ${LOCAL_DIR}/registry.yaml"
./bin/mcpdoll snapshot build \
  --registry "${LOCAL_DIR}/registry.yaml" \
  --key "${LOCAL_DIR}/${KEY_ID}.key" \
  --key-id "${KEY_ID}" \
  --out "${LOCAL_DIR}/snapshot.pb"

# ------------------------------------------------------------- data plane ----

info "starting the data plane on :8080"
# The trusted key is passed by environment rather than written into the config,
# so the committed config file holds no deployment-specific material.
MCPDOLL_DATAPLANE_TRUSTED_SIGNING_KEYS="${TRUST_ENTRY}" \
MCPDOLL_REQUEST_STATE_KEY="${MCPDOLL_REQUEST_STATE_KEY:-local-dev-request-state-key-32-bytes!!}" \
  ./bin/mcpdoll-dp -config "${LOCAL_DIR}/dataplane.yaml" \
  > "${LOG_DIR}/mcpdoll-dp.log" 2>&1 &
DP_PID=$!
record_pid "${DP_PID}"
wait_http http://localhost:8080/readyz mcpdoll-dp "${DP_PID}" \
  || die "the data plane failed to start"
echo

# ---------------------------------------------------------------- console ----

# The console is optional: everything above is usable from the CLI, and a
# missing node_modules should slow nobody down who is working on the Go side.
if [ -d web/node_modules ]; then
  info "starting the console on :5173"
  ( cd web && npm run dev -- --port 5173 ) > "${LOG_DIR}/console.log" 2>&1 &
  CONSOLE_PID=$!
  record_pid "${CONSOLE_PID}"
  wait_http http://localhost:5173/ console "${CONSOLE_PID}" \
    || warn "the console did not start; see ${LOG_DIR}/console.log"
  echo
else
  warn "web/node_modules is absent — skipping the console. Run 'cd web && npm install'."
  CONSOLE_PID=""
fi

# ------------------------------------------------------------------ ready ----

cat <<BANNER

MCPDoll is up.

  Data plane      http://localhost:8080/mcp   (one endpoint; the key names the tenant)
  Control plane   http://localhost:3001       (token: ${MCPDOLL_CP_TOKEN})
  Console         http://localhost:5173
  Grafana         http://localhost:3300       (folder: MCPDoll)
  Credentials     ${KEYS}
  Logs            ${LOG_DIR}/

Try it. Everything below presents an agent key — there is no audience to pick and
no subject to claim, because the tenant and the toolset both come from the key:

  ACME=\$(awk '/acme\/support/ {print \$2}' ${KEYS})
  GLOBEX=\$(awk '/globex\/support/ {print \$2}' ${KEYS})

  # What a support agent sees. Eight tools: CRM, HR, warehouse.
  ./bin/mcpdoll gateway catalog --as "\${ACME}"

  # The same toolset name in another tenant: four tools, from a different
  # backend deployment. That is ADR 0017 in one command.
  ./bin/mcpdoll gateway catalog --as "\${GLOBEX}"

  # Call a tool across two backends
  ./bin/mcpdoll gateway call crm.lookup_customer --as "\${ACME}" \\
      --args '{"customer_id":"cus_1"}'
  ./bin/mcpdoll gateway call hr.lookup_employee --as "\${ACME}" \\
      --args '{"staff_number":"E-1"}'

  # A destructive tool: policy asks for confirmation, and the envelope binds
  # the tenant, so the approval cannot be replayed anywhere else.
  PLATFORM=\$(awk '/platform@acme/ {print \$2}' ${KEYS})
  ./bin/mcpdoll gateway call dep.promote_release --as "\${PLATFORM}" \\
      --args '{"build":"v1"}' --output json

  # The redact plugin at work: the backend returns a card number, the model does not see it
  ./bin/mcpdoll gateway call crm.get_payment_method --as "\${ACME}" \\
      --args '{"customer_id":"cus_1"}'

  # Who can reach what, and change it. A grant takes effect at the next
  # snapshot, so the rebuild is part of the operation rather than a follow-up.
  ./bin/mcpdoll tenants list
  ./bin/mcpdoll users grants support@acme.example --tenant acme
  ./bin/mcpdoll users grants set support@acme.example --tenant acme \\
      --grant tool_user@t/acme/ts/support/lookup_customer
  ./bin/mcpdoll snapshot build -r ${LOCAL_DIR}/registry.yaml \\
      --key ${LOCAL_DIR}/${KEY_ID}.key --key-id ${KEY_ID} \\
      --out ${LOCAL_DIR}/snapshot.pb
  ./bin/mcpdoll gateway catalog --as "\${ACME}"   # one tool now

  # The entitlements plugin ships in SHADOW: it records what it would hide
  # without hiding it. Watch it decide, then promote it:
  grep "shadow verdict diverged" ${LOG_DIR}/mcpdoll-dp.log
  #   ... then set \`rollout: enforce\` on plg_entitlements in registry.yaml
  #   and rebuild the snapshot; the catalog changes.

  # What is actually in the snapshot
  ./bin/mcpdoll snapshot inspect ${LOCAL_DIR}/snapshot.pb --tools

  # The same answers over the API the console uses
  curl -s -H "Authorization: Bearer ${MCPDOLL_CP_TOKEN}" \\
      http://localhost:3001/api/v1/registry | jq .

Republish after editing ${LOCAL_DIR}/registry.yaml, or after changing a grant.
The version is assigned by the build, so there is nothing to bump:

  ./bin/mcpdoll snapshot build -r ${LOCAL_DIR}/registry.yaml \\
      --key ${LOCAL_DIR}/${KEY_ID}.key --out ${LOCAL_DIR}/snapshot.pb

The data plane picks it up within a few seconds, with no restart.

Stop everything with: make dev-down

BANNER
