#!/bin/sh
# Create the demo tenants, users, grants, and API keys — then build the first
# snapshot from them.
#
# The tenancy half goes through the CLI against the API rather than against the
# database. That is deliberate: it is the same path an operator uses, so a
# broken command fails here rather than the first time somebody types it.
#
# The snapshot build is here rather than in bootstrap.sh because a snapshot
# carries tenants and principals, and those do not exist until the lines above
# have run. Building earlier would need a control plane that needs a data plane
# that needs the snapshot.
#
# Idempotent by check-then-create. Re-running rebuilds the snapshot and mints no
# keys — a secret is shown once, and a second run cannot show it again because
# nothing kept it.
set -eu

STATE=/srv/state
export MCPDOLL_TOKEN="${MCPDOLL_CP_TOKEN:-dev-token-not-a-secret}"

log() { printf '\033[1;35mseed\033[0m %s\n' "$*"; }

# A tenant the registry merely *binds* is listed too, with status
# "unregistered" and no id. Matching on the slug alone would see that row and
# skip creating the record — leaving a tenant nothing can authenticate into,
# which is exactly the state the listing exists to make visible.
have_tenant() {
  mcpdoll tenants list --output json \
    | tr -d ' \n' \
    | grep -q "\"slug\":\"$1\",\"name\":[^}]*\"status\":\"active\""
}

have_user() {
  mcpdoll users list --tenant "$1" --output json 2>/dev/null \
    | grep -q "\"email\": \"$2\""
}

# The keys land in a file rather than only on stdout, because the console's
# catalog and playground screens ask for one and re-reading compose logs to find
# it is a bad first five minutes.
KEYS="${STATE}/demo-keys.txt"

mint() {
  tenant="$1"; email="$2"; name="$3"; shift 3
  secret="$(mcpdoll users keys mint "${email}" \
    --tenant "${tenant}" --name "${name}" --output json "$@" \
    | sed -n 's/.*"secret": "\([^"]*\)".*/\1/p')"
  [ -n "${secret}" ] || { echo "seed: minting ${name} produced no secret" >&2; exit 1; }
  printf '%-24s %-28s %s\n' "${tenant}/${email}" "${name}" "${secret}" >> "${KEYS}"
  log "minted ${name} for ${email} (${tenant})"
}

# --------------------------------------------------------------- tenants ----

# acme and globex are the two the registry binds. Creating them here is what
# turns the registry's tenant slugs from routing labels into things a person can
# authenticate into.
for tenant in acme globex; do
  if have_tenant "${tenant}"; then
    log "tenant ${tenant} already exists"
  else
    log "creating tenant ${tenant}"
    mcpdoll tenants create "${tenant}" --name "$(echo "${tenant}" | tr 'a-z' 'A-Z')" >/dev/null
  fi
done

# ----------------------------------------------------------------- users ----

: > "${KEYS}.new"

seed_user() {
  tenant="$1"; email="$2"; label="$3"; shift 3

  if have_user "${tenant}" "${email}"; then
    log "user ${email} already exists in ${tenant}"
    return 0
  fi

  log "creating ${email} in ${tenant}"
  mcpdoll users create "${email}" --tenant "${tenant}" --name "${label}" \
    --password "demo-password-not-a-secret" >/dev/null

  # Grants come second and separately, because a new user holding nothing is
  # the correct starting state — an account that could reach tools the moment
  # it existed would make onboarding the thing that grants access.
  mcpdoll users grants set "${email}" --tenant "${tenant}" "$@" >/dev/null
  log "granted ${email}: $*"
  echo "${tenant} ${email}" >> "${KEYS}.new"
}

# A support agent: the everyday catalog, nothing destructive.
seed_user acme support@acme.example "Support Agent" \
  --grant "tool_user@t/acme/ts/support"

# A platform operator: the same, plus the deploy tools, which are destructive
# and therefore go through MRTR confirmation on every call.
seed_user acme platform@acme.example "Platform Operator" \
  --grant "tool_user@t/acme/ts/support" \
  --grant "tool_user@t/acme/ts/platform"

# A security researcher: only the hostile backend. Granted by toolset name, so
# nothing else in acme can reach it and it cannot reach anything else.
seed_user acme research@acme.example "Threat Researcher" \
  --grant "tool_user@t/acme/ts/untrusted"

# Globex's support agent. Same role, same toolset name, different tenant — and
# therefore a different backend deployment behind identical tool names. This is
# the pair to compare in the catalog screen.
seed_user globex support@globex.example "Support Agent" \
  --grant "tool_user@t/globex/ts/support"

# ------------------------------------------------------------------ keys ----

if [ -s "${KEYS}.new" ]; then
  {
    printf '# MCPDoll demo credentials, minted %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf '# Every secret here is shown once and nothing keeps it. Paste one into\n'
    printf '# the console at /gateway/catalog to see exactly what that agent sees.\n\n'
  } > "${KEYS}"

  while read -r tenant email; do
    mint "${tenant}" "${email}" "agent"
  done < "${KEYS}.new"

  log "credentials written to ${KEYS}"
  cat "${KEYS}"
else
  log "every demo user already existed; no keys minted"
fi
rm -f "${KEYS}.new"

# -------------------------------------------------------------- snapshot ----

# The build assigns the version itself — a Unix timestamp, monotonic without
# anybody coordinating. Stamping one into the registry here used to be
# necessary and is now the bug it was working around: the console's rebuild
# would have reused whatever was stamped and been silently refused.
KEY_ID="${MCPDOLL_KEY_ID:-dev}"
log "building a snapshot"

# --database-url is what carries the tenants and the API key digests into the
# artifact. Without it the build fails on the first binding, correctly: its
# tools would be admitted for a tenant no principal could belong to.
mcpdoll snapshot build \
  --registry "${STATE}/registry.yaml" \
  --key "${STATE}/${KEY_ID}.key" \
  --key-id "${KEY_ID}" \
  --database-url "${MCPDOLL_DATABASE_URL}" \
  --out "${STATE}/snapshot.pb"

log "done"
