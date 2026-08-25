# MCPDoll

An enterprise gateway for the [Model Context Protocol](https://modelcontextprotocol.io).
One MCP endpoint in front of many backends, where what an agent sees is decided
by who holds its credential.

```
agents ──► /mcp ──► MCPDoll ──► crm.acme.example
                       │        hr.internal
                       │        warehouse.example
                       │
                    snapshot (signed)
                       ▲
                  control plane ──► registry.yaml + Postgres
```

## What problem it solves

Point an agent at five MCP servers directly and you get five connections, five
tool namespaces that can collide, no way to say "this team may read customers
but not delete them", and no answer to *what could this agent do last Tuesday*.

MCPDoll puts one endpoint in front of them. Tool definitions are **admitted**
into a signed snapshot rather than proxied live, so what a client sees is a
reviewed artifact — and a backend that changes its catalog underneath is a
detectable event rather than a silent change to every agent's prompt.

Three ideas do most of the work:

**Tenants own users; users hold grants; grants name toolsets.** A grant is a
role at a hierarchical scope — `*` ⊃ `t/acme` ⊃ `t/acme/ts/support` ⊃
`t/acme/ts/support/lookup_customer`. There is no audience to pick and no subject
to claim: the tenant and the toolset both come from the credential presented.

**One backend serves many tenants at different addresses.** The same toolset
name resolves to Acme's CRM for Acme and Globex's for Globex — same tool names,
different data, admitted separately so each tenant's catalog can follow its own
backend's version.

**The data plane never asks the control plane anything.** Grants, tools, and
credential digests all travel in one signed snapshot, so a control-plane outage
is invisible to a tool call. The single exception is revocation, which gets its
own signed artifact because "a few seconds" is the wrong answer to a leaked key.

## The three laws

Enforced mechanically, not by convention:

1. **Tri-surface.** A feature is not done until it is reachable from the API,
   the CLI, and the console. `make parity` reads the OpenAPI document, the built
   binary's command tree, and a manifest generated from the router — three real
   artifacts, never a hand-maintained list — and fails the build if any
   operation is missing a surface.
2. **Working at every commit.** `make test` is green on every commit in this
   history.
3. **No stubs.** No `TODO`, no `panic("not implemented")`, no fake data.
   Unfinished work is written down in [`docs/deferred.md`](docs/deferred.md)
   with what is missing and why, rather than sketched in code.

## Running it

```sh
make up      # the whole stack in Docker, healthy before it says it is
make ps      # what is running
make down    # stop, keeping the database and the signing key
```

`make up` starts Postgres, six fixture MCP backends, both planes, the console,
and an LGTM observability stack.

| | |
|---|---|
| Console | http://localhost:5173 — start at `/overview` |
| Data plane | http://localhost:8080/mcp — one endpoint; the key names the tenant |
| Control plane | http://localhost:3001 |
| Inspector | http://localhost:6274 — the reference MCP Inspector, pointed here |
| Grafana | http://localhost:3300 — folder `MCPDoll` |

### Signing in

Every credential below is a **development** value, seeded by
`deploy/docker/seed.sh` and never by the product. They are named so that
anything finding `demo-password-not-a-secret` in a real database has a finding
rather than a mystery.

| Email | Password | What it can do |
|---|---|---|
| `dev-admin@mcpdoll.local` | `demo-password-not-a-secret` | Everything. **Start here.** |
| `platform@acme.example` | `demo-password-not-a-secret` | Tenant admin for `acme` |
| `support@acme.example` | `demo-password-not-a-secret` | Tool access only — signs in, sees nothing |

Email and password, no tenant. A user is a person and belongs to no tenant;
which tenants they reach is what their grants say. The tenant is a property of
the *credential* an agent presents — an API key names the one tenant its MCP
session resolves to — not of the person signing in.

The second and third are worth a look precisely because they are *restricted*.
Signed in as `platform@acme.example`, `/tenants` lists only `acme` — the control
plane filters rather than refusing — and the registry and gateway screens return
403, because those are org-wide and a tenant admin has no business reading
another tenant's backend addresses. Signed in as `support@acme.example`, almost
everything is refused, which is the correct state for an account granted tool
access and nothing else.

There is also a `platform_admin` created on first boot with a *generated*
password printed once to the control plane's stderr. That is the production
path; the seeded admin above exists because a dev container gets recreated and
that line scrolls away.

The deployment token (`dev-token-not-a-secret`) is an API and CLI credential —
it is not a way to sign in to the console, and the login screen no longer offers
it. It exists so CI can build a snapshot before any user does.

### Agent credentials

Four agent keys are minted at seed time and written to the state volume:

```sh
docker exec mcpdoll-cp cat /srv/state/demo-keys.txt
```

Paste one into the console at `/gateway/catalog` to see exactly what that agent
sees. They are real, working secrets — which is why that file is gitignored, and
why this whole section says *development* twice.

The claim in one command, twice:

```sh
export MCPDOLL_TOKEN=dev-token-not-a-secret
ACME=$(docker exec mcpdoll-cp awk '/acme\/support/ {print $2}' /srv/state/demo-keys.txt)
GLOBEX=$(docker exec mcpdoll-cp awk '/globex\/support/ {print $2}' /srv/state/demo-keys.txt)

./bin/mcpdoll gateway catalog --as "$ACME"     # 8 tools: crm, hr, warehouse
./bin/mcpdoll gateway catalog --as "$GLOBEX"   # 4 tools, from a different container
```

Same toolset name, same tool names, different backend deployment.

### Talking to the gateway directly

The endpoint is MCP over streamable HTTP — JSON-RPC in, Server-Sent Events out.
No handshake: the gateway is stateless, so `tools/list` works on a bare request.
`Accept` must name both types, which the protocol requires.

```sh
KEY=$(docker exec mcpdoll-cp awk '/acme\/support/ {print $2}' /srv/state/demo-keys.txt)

# What this key can see. There is no tenant or toolset in the request —
# both come from the credential.
curl -sS -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' \
  | sed -n 's/^data: //p' | jq '.result | {ttlMs, cacheScope, tools: [.tools[].name]}'
```

```json
{
  "ttlMs": 300000,
  "cacheScope": "private",
  "tools": ["crm.get_payment_method", "crm.list_open_tickets", "crm.lookup_customer",
            "crm.update_customer", "hr.get_org_chart", "hr.lookup_employee",
            "whs.check_stock", "whs.reserve_stock"]
}
```

Calling one:

```sh
curl -sS -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
       "params":{"name":"crm.lookup_customer","arguments":{"customer_id":"cus_1"}}}' \
  | sed -n 's/^data: //p' | jq -r '.result.content[].text'
```

The `sed` strips the SSE framing (`data: ` on each line). Every response also
names who the gateway decided you are, which the request could not have said:

```sh
curl -sS -D- -o /dev/null -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' \
  | grep -i '^x-mcpdoll'
#   X-Mcpdoll-Tenant: acme
#   X-Mcpdoll-Subject-Resolved: support@acme.example
```

A revoked or unknown key gets `401 unauthenticated`; a key whose principal the
serving snapshot does not carry gets `403` with the reason.

`make dev` runs the same stack as host processes instead, which is a faster
edit-to-running loop for Go changes.

## Architecture

Two planes, and which is which matters.

The **control plane** owns the registry (a reviewable YAML document), the
database (tenants, users, grants, API keys), and the signing key. It resolves
all of that into a signed snapshot. It is never in an agent's request path.

The **data plane** serves from one snapshot. It authenticates a credential
against digests the snapshot carries, composes that principal's catalog from
their grants on first connect, runs a seven-hook plugin pipeline around every
call, and reaches backends through per-tenant pools. It holds no database
connection and makes no call to the control plane to answer a request.

Some things worth knowing before reading the code:

- **Admitted, not proxied.** `tools/list` serves definitions from the snapshot.
  A backend whose catalog has drifted is quarantined or recorded depending on
  its serving mode; clients never see live backend output.
- **Every catalog is `cacheScope: private`.** It is derived from one principal's
  grants, so no two principals necessarily see the same list and none of it may
  be shared from a common cache.
- **No token passthrough, ever.** The identity resolver returns a principal and
  deliberately not the inbound credential, which makes passthrough structurally
  impossible rather than merely discouraged. Backends get RFC 8693 exchange or
  nothing.
- **Two hash functions, on purpose.** Passwords are Argon2id; API key secrets
  are SHA-256. A KDF defends a secret a human chose; a key secret is 192 CSPRNG
  bits, and a memory-hard hash on the data plane's request path would be a
  denial-of-service primitive pointed at ourselves.

## Reading further

[`docs/adr/`](docs/adr/) is the design of record — 23 decisions, each with what
was rejected and why. The ones that explain the most:

| | |
|---|---|
| [0002](docs/adr/0002-control-data-plane-split.md) | Why the data plane never calls the control plane |
| [0006](docs/adr/0006-admission-not-proxy.md) | Admitted definitions, not live proxying |
| [0015](docs/adr/0015-rbac-scopes-and-engines.md) | Hierarchical scopes, and two engines pinned to identical decisions |
| [0016](docs/adr/0016-toolsets-replace-audiences.md) | Why audiences were removed |
| [0018](docs/adr/0018-grants-in-the-snapshot.md) | Grants in the signed artifact, and what that costs |
| [0021](docs/adr/0021-offline-credential-verification.md) | Verifying a credential with no database |
| [0023](docs/adr/0023-out-of-band-revocation.md) | Revocation that does not wait for a snapshot |

[`PROGRESS.md`](PROGRESS.md) is what is built, slice by slice, including a list
of bugs that only surfaced because the tests run against real backends and a
real database rather than mocks.

[`docs/deferred.md`](docs/deferred.md) is the honest other half: what is not
built, what is partly built, and the one place where a design trade leaves a
measurable exposure rather than closing it.

## License

MIT — see [LICENSE](LICENSE). Same terms as
[ragdoll](https://github.com/japhar81/ragdoll), which this borrows its shape
from (ADR 0001).

## Status

Not deployed anywhere, and not ready to be. The largest gaps are an identity
provider (people sign in with local passwords today), the admission pipeline and
its approval workflow, and a Helm chart. `docs/deferred.md` is the full list.
