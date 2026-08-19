#!/bin/sh
# Start the data plane with the trust anchor the bootstrap generated.
#
# The public key is read from the shared volume and exported, rather than baked
# into the image or the config file. That mirrors how a real deployment injects
# it — from a secret store at start — and it means the committed config holds no
# deployment-specific material.
set -eu

STATE=/srv/state
KEY_ID="${MCPDOLL_KEY_ID:-dev}"
PUB="${STATE}/${KEY_ID}.pub"

if [ ! -f "${PUB}" ]; then
  echo "entrypoint: ${PUB} is missing — the bootstrap did not run" >&2
  exit 2
fi

# A data plane with no trusted key would refuse every snapshot, including the
# one just built for it, and report it as a signature failure. Failing here
# instead says what is actually wrong.
MCPDOLL_DATAPLANE_TRUSTED_SIGNING_KEYS="${KEY_ID}:$(tr -d '\n' < "${PUB}")"
export MCPDOLL_DATAPLANE_TRUSTED_SIGNING_KEYS

exec mcpdoll-dp -config /srv/src/dataplane.yaml "$@"
