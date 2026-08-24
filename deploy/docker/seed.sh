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
  tenant="$1"; email="$2"; name="$3"
  secret="$(mcpdoll users keys mint "${email}" \
    --tenant "${tenant}" --name "${name}-$(date +%s)" --output json \
    | sed -n 's/.*"secret": "\([^"]*\)".*/\1/p')"
  [ -n "${secret}" ] || { echo "seed: minting a key for ${email} produced no secret" >&2; exit 1; }
  printf '%-30s %s\n' "${tenant}/${email}" "${secret}" >> "${KEYS}"
  log "minted a key for ${email} (${tenant})"
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

# Users and keys are separate concerns. A user survives across runs — they are
# rows in a database this container does not own — but the *keys file* lives on
# the state volume, and a key's secret cannot be recovered once it is gone. So:
# create users that are missing, and mint a key whenever the file is absent,
# whether or not the user was new. A user may hold several keys, and the
# alternative is a banner pointing at a file that does not exist.
#
#   tenant|email|display name|comma-separated role@scope grants
DEMO_USERS="\
acme|support@acme.example|Support Agent|tool_user@t/acme/ts/support
acme|platform@acme.example|Platform Operator|tool_user@t/acme/ts/support,tool_user@t/acme/ts/platform
acme|research@acme.example|Threat Researcher|tool_user@t/acme/ts/untrusted
globex|support@globex.example|Support Agent|tool_user@t/globex/ts/support"

echo "${DEMO_USERS}" | while IFS='|' read -r tenant email label grants; do
  [ -n "${tenant}" ] || continue
  if have_user "${tenant}" "${email}"; then
    log "user ${email} already exists in ${tenant}"
    continue
  fi

  log "creating ${email} in ${tenant}"
  mcpdoll users create "${email}" --tenant "${tenant}" --name "${label}" \
    --password "demo-password-not-a-secret" >/dev/null

  # Grants come second and separately, because a new user holding nothing is
  # the correct starting state — an account that could reach tools the moment
  # it existed would make onboarding the thing that grants access.
  set -- ""
  shift
  for g in $(echo "${grants}" | tr ',' ' '); do
    set -- "$@" --grant "${g}"
  done
  mcpdoll users grants set "${email}" --tenant "${tenant}" "$@" >/dev/null
  log "granted ${email}: ${grants}"
done

# ----------------------------------------------------------- console admin --

# Somebody who can actually use the console.
#
# `SeedPlatformAdmin` already creates admin@mcpdoll.local with a *generated*
# password printed once to stderr — correct for production and useless for a dev
# stack, where the container has been recreated a dozen times and that line is
# long gone. So this seeds a second administrator with a password the README can
# name.
#
# Only ever run by the dev stack. The password is deliberately self-describing:
# anything reading `demo-password-not-a-secret` in a production database is a
# finding, not a mystery.
# In `acme`, not `platform`. Grants are global either way — this account
# administers everything — but a principal's *catalog* comes from their own
# tenant, and `platform` has no backend bindings. An administrator whose
# gateway view is permanently empty is a bad first five minutes.
if have_user acme dev-admin@mcpdoll.local; then
  log "console admin already exists"
else
  log "creating the console admin"
  mcpdoll users create dev-admin@mcpdoll.local --tenant acme \
    --name "Console Admin" --password "demo-password-not-a-secret" >/dev/null
  mcpdoll users grants set dev-admin@mcpdoll.local --tenant acme \
    --grant "platform_admin@*" >/dev/null
fi

# ------------------------------------------------------------------ keys ----

if [ ! -s "${KEYS}" ]; then
  {
    printf '# MCPDoll demo credentials, minted %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    printf '# Every secret here is shown once and nothing keeps it. A previous\n'
    printf '# run'"'"'s keys are not recoverable, so this run minted new ones.\n'
    printf '#\n'
    printf '# Paste one into the console at /gateway/catalog to see exactly what\n'
    printf '# that agent sees.\n\n'
  } > "${KEYS}"

  echo "${DEMO_USERS}" | while IFS='|' read -r tenant email label grants; do
    [ -n "${tenant}" ] || continue
    mint "${tenant}" "${email}" "agent"
  done

  log "credentials written to ${KEYS}"
  cat "${KEYS}"
else
  log "reusing the credentials in ${KEYS}"
fi

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
