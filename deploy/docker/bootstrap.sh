#!/bin/sh
# Prepare the shared state the data plane and control plane serve from.
#
# Runs once, to completion, after the fixture backends are healthy and before
# anything that serves starts. Compose enforces that ordering with
# `service_completed_successfully`, which is the whole reason this is a separate
# container rather than an entrypoint that races.
#
# Idempotent: re-running it regenerates the snapshot against whatever the
# fixtures currently publish, and leaves an existing signing key alone.
set -eu

STATE=/srv/state
SRC=/srv/src
KEY_ID="${MCPDOLL_KEY_ID:-dev}"

log() { printf '\033[1;34mbootstrap\033[0m %s\n' "$*"; }

mkdir -p "${STATE}"

# ------------------------------------------------------------------ keys ----

if [ ! -f "${STATE}/${KEY_ID}.key" ]; then
  log "generating a snapshot signing keypair (${KEY_ID})"
  mcpdoll keys generate --dir "${STATE}" --key-id "${KEY_ID}" --quiet >/dev/null
else
  log "reusing the existing signing key (${KEY_ID})"
fi

# ------------------------------------------------------------- registry -----

# The committed registry is the single source. Two things differ inside the
# compose network and are substituted here rather than maintained as a second
# document: backend endpoints are container names, and the plugin artifacts are
# at an absolute path in the image.
#
# Keeping the mapping here — beside the compose file that defines those names —
# is why there is no second registry.yaml to drift out of agreement.
log "resolving the registry for the compose network"
sed \
  -e 's|http://localhost:9101|http://fixture-crm:9101|' \
  -e 's|http://localhost:9102|http://fixture-hr:9102|' \
  -e 's|http://localhost:9103|http://fixture-warehouse:9103|' \
  -e 's|http://localhost:9104|http://fixture-websearch:9104|' \
  -e 's|http://localhost:9105|http://fixture-deploy:9105|' \
  -e 's|file://deploy/local/plugins/|file:///srv/plugins/|' \
  "${SRC}/registry.yaml" > "${STATE}/registry.yaml"

# Every localhost endpoint must have been rewritten. A backend the substitution
# missed would be discovered against the *bootstrap container's* own loopback,
# where nothing is listening — and the failure would read as an unreachable
# backend rather than as a stale mapping.
if grep -q 'endpoint: http://localhost' "${STATE}/registry.yaml"; then
  echo "bootstrap: a backend endpoint was not rewritten for the compose network:" >&2
  grep -n 'endpoint: http://localhost' "${STATE}/registry.yaml" >&2
  echo "bootstrap: add it to the sed mapping in deploy/docker/bootstrap.sh" >&2
  exit 1
fi

# ------------------------------------------------------------- plugins ------

# Stamp each plugin's digest. The digest is what makes a swapped artifact fail
# closed, so it cannot be a constant in a committed file — it changes whenever
# the plugin does. Stamping here means editing a plugin and rebuilding just
# works, rather than failing with a mismatch that looks like an attack.
for plugin in redact entitlements; do
  artifact="/srv/plugins/${plugin}.wasm"
  [ -f "${artifact}" ] || { echo "bootstrap: ${artifact} is missing" >&2; exit 1; }

  digest="sha256:$(sha256sum "${artifact}" | cut -d' ' -f1)"
  log "stamping ${plugin} ${digest}"

  # Rewrite the artifact_digest on the line *after* this plugin's artifact_ref,
  # so two plugins cannot get each other's digest.
  awk -v marker="artifact_ref: file:///srv/plugins/${plugin}.wasm" \
      -v digest="${digest}" '
    {
      if (stamp == 1 && $0 ~ /artifact_digest:/) {
        match($0, /^[ \t]*/)
        printf "%sartifact_digest: \"%s\"\n", substr($0, 1, RLENGTH), digest
        stamp = 0
        next
      }
      stamp = 0
      if (index($0, marker) > 0) { stamp = 1 }
      print
    }
  ' "${STATE}/registry.yaml" > "${STATE}/registry.yaml.tmp"
  mv "${STATE}/registry.yaml.tmp" "${STATE}/registry.yaml"
done

# ------------------------------------------------------------ snapshot ------

# A fresh version on every run. The store refuses a snapshot no newer than the
# one it is serving, so a rebuild that reused version 1 would be silently
# ignored and the stack would keep serving stale configuration.
VERSION="$(date +%s)"
log "building snapshot version ${VERSION}"
sed -i "s/^version: [0-9]*/version: ${VERSION}/" "${STATE}/registry.yaml"

mcpdoll snapshot build \
  --registry "${STATE}/registry.yaml" \
  --key "${STATE}/${KEY_ID}.key" \
  --key-id "${KEY_ID}" \
  --out "${STATE}/snapshot.pb"

log "ready: $(ls -1 ${STATE} | tr '\n' ' ')"
