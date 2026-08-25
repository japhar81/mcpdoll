#!/bin/sh
# The reference MCP Inspector, made to open in the console's theme.
#
# The Inspector is a Mantine app booted with `defaultColorScheme: "auto"`, so it
# follows the operating system. The console does not — it has one palette, a
# light one. On a machine set to dark that produced two different darks side by
# side in the same iframe, which is the mismatch this fixes.
#
# There is no flag and no query parameter for it. Mantine reads the scheme from
# `localStorage` under `mantine-color-scheme-value`, on the Inspector's own
# origin, which the console cannot write to across origins. So the value is
# planted by a script injected into the Inspector's own `index.html`, which runs
# before its bundle does.
#
# Only when nothing is stored yet. The Inspector has a theme toggle in its
# toolbar, and overwriting on every load would leave that toggle looking broken:
# it would visibly work and then revert on the next refresh. This sets the
# default and then stays out of the way.
set -eu

VERSION=2.3.0

# Installed at a fixed prefix rather than run through `npx`. npx unpacks into a
# content-hashed path under ~/.npm/_npx, which is exactly the wrong thing to aim
# a `sed` at — the hash changes when the version does, and the patch would
# silently stop applying while everything still started normally. Pinning also
# means the file being patched is the file that was inspected.
npm install -g --silent "@modelcontextprotocol/inspector@${VERSION}"

INDEX="$(find /usr/local/lib/node_modules/@modelcontextprotocol/inspector \
  -path '*clients/web/dist/index.html' | head -1)"

# Loud, not silent. A missing file here means a newer Inspector moved its client
# and the theme fix has quietly stopped working — which would look exactly like
# it working, until somebody opened it on a dark machine.
if [ -z "${INDEX}" ]; then
  echo "inspector: cannot find the web client's index.html to theme" >&2
  exit 1
fi

if ! grep -q 'mcpdoll-theme' "${INDEX}"; then
  sed -i "s|</head>|<script id=\"mcpdoll-theme\">try{var k='mantine-color-scheme-value';if(!localStorage.getItem(k))localStorage.setItem(k,'light')}catch(e){}</script></head>|" "${INDEX}"
  grep -q 'mcpdoll-theme' "${INDEX}" || {
    echo "inspector: theming the client's index.html did not take" >&2
    exit 1
  }
  echo "inspector: client themed to match the console"
fi

exec mcp-inspector --web --transport http --server-url http://dataplane:8080/mcp
