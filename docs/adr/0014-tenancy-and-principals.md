# ADR 0014: Tenancy, Principals, and Agent Credentials

## Status

**Accepted, with one decision reversed.** Supersedes the single-org assumption
baked into `registry.org`. The reversal — users no longer belong to a tenant —
is recorded in [Amendment: users are global](#amendment-users-are-global) at the
bottom of this ADR and implemented by migration `0004_users_are_global.sql`.

Companion ADRs: [0015](./0015-rbac-scopes-and-engines.md) (RBAC),
[0016](./0016-toolsets-replace-audiences.md) (toolsets),
[0020](./0020-pluggable-identity-and-authz.md) (pluggable providers).

## Context

MCPDoll was built single-tenant. One `org` field, one registry document, and a
set of *audiences* declared by an operator — `support-agents`,
`platform-agents` — each published at its own URL. Which tools you saw was
decided by which URL you connected to; identity only gated whether you were
allowed to connect at all.

That does not survive contact with the actual requirement: one MCPDoll instance
serving many tenants, where a tenant's users get tools drawn from that tenant's
own backends, and an admin hands out access per user.

Three things follow from multi-tenancy that the old model has no answer for:

1. **The same logical backend is a different host per tenant.** Tenant Acme's
   CRM lives at `acme.realapp.com`; tenant Globex's at `globex.realapp.com`.
   The old `servers` list has exactly one `endpoint` per backend.
2. **Access is per user, not per URL.** "Give Alice the CRM lookup tool" has no
   expression in a model where access is a URL you know or do not know.
3. **Agents are not people.** A human signs in through an identity provider. An
   unattended agent holds a credential. Both need to resolve to a principal
   whose toolset is computable, and the agent's should be able to be *narrower*
   than the human's.

RAGdoll solved the same problem and MCPDoll is meant to be its family member
(ADR 0001). Its shape is adopted here rather than reinvented.

## Decision

### Tenants own users; users hold grants; keys narrow them

```
tenant  ─┬─ user ─┬─ user_identity   (federated: one per IdP subject)
         │        ├─ rbac_grant      (role @ scope)
         │        └─ api_key ─── key_grant   (a SUBSET of the user's)
         └─ backend_binding          (this tenant's hosts — ADR 0017)
```

A **tenant** is the isolation boundary. Every user, every grant, every backend
binding, and every published tool belongs to exactly one, and nothing crosses
without a grant at global scope (`*`), which only a platform admin holds.

A **user** is a person or a service identity inside a tenant. Local password
credentials and federated identities both land here;
`user_identities (provider, subject)` links an external subject to a local user
so the same person arriving through two IdPs is still one user with one set of
grants. Directly ported from RAGdoll's `005_rbac_identity.sql`.

An **API key** is how an agent authenticates. It belongs to a user and carries
its own grant list.

### A key may narrow, never widen

A key's effective grants are `key_grants ∩ owner_grants`, recomputed at
resolution rather than at mint time.

The intersection matters. Minting is not the last word: a user's grants can be
reduced after a key is issued, and a key that kept what it was minted with
would be a way to hold access the admin believes they revoked. Intersecting on
every resolution means **revoking the user revokes every key they hold**, with
no key-by-key cleanup and no possibility of missing one.

```
alice holds:  crm.*, hr.*, dep.promote_release

key "support-bot"  declares: crm.lookup_customer      -> effective: crm.lookup_customer
key "deploy-bot"   declares: dep.promote_release      -> effective: dep.promote_release
key "greedy"       declares: crm.*, billing.*         -> effective: crm.*
                                                          (billing was never alice's to give)

admin revokes alice's crm.*  ->  support-bot resolves to nothing, immediately
```

A key that declares more than its owner holds is not an error at mint time —
the owner's grants can legitimately shrink later, and failing the mint would
only push the same situation to a moment nobody is watching. It is silently
narrowed at resolution, and the console shows the *effective* set beside the
declared one so the gap is visible rather than surprising.

### Credentials are stored as `prefix` + hash

Following RAGdoll's `api_keys`: a lookup-able public prefix and an Argon2id
hash of the remainder. The plaintext is shown once at mint and never again,
and the prefix is what appears in an audit trail — enough to identify which key
acted, useless for acting as it.

### JIT provisioning is a setting, not a default

`auth_settings.signup_mode` — `admin_only` | `open_default_role` |
`open_no_access` — ported from RAGdoll unchanged. The default is `admin_only`:
a fresh install does not let anyone who can reach the IdP create themselves an
account. `open_no_access` is the useful middle ground for SSO — the user is
created on first login and holds nothing until an admin grants something.

## Alternatives considered

- **Tenant as a scope on a flat user table**, with no tenant ownership.
  Rejected: it makes "delete this tenant" a query rather than a cascade, and
  every listing becomes a filter somebody can forget to apply. Ownership makes
  the boundary structural. **This rejection was wrong — see the amendment
  below.** The cost it names is real and small; what it did not weigh is what
  tenant-owned users do to the login screen.
- **Keys as independent service-account principals.** Genuinely cleaner for
  unattended agents, and rejected only because it adds a second principal type
  to the RBAC model and to every screen that answers "who did this". A key
  belonging to a user gives one answer to that question. Revisit if service
  identities need to outlive the person who created them — which they will,
  eventually, and this ADR should be reopened rather than worked around.
- **Keys carrying exactly their owner's grants.** Simpler, and rejected because
  it makes one leaked agent credential equal to its owner's full access. The
  intersection costs one step at resolution.
- **Widening keys** (a key with grants its owner lacks). Rejected outright: it
  is a privilege-escalation primitive wearing a convenience costume. An admin
  who wants an agent to have more should grant it to a user made for that
  agent.

## Consequences

- **A database is now required.** Users, grants, keys and identities cannot
  live in a committed YAML — they are per-install state, some of it secret, and
  they change far more often than a registry document. Postgres arrives with
  this ADR; the GitOps registry survives for what it was always good at
  (which backends exist, how tools are classified).
- **`registry.org` is replaced by the tenant table.** Existing single-tenant
  installs migrate to one tenant whose slug is the old org id.
- **Revoking a user is now the complete operation** it appears to be. This is
  a property worth protecting: any future change that caches effective key
  grants must invalidate on the owner's grants changing, or it silently breaks
  the guarantee above.
- The console gains tenant, user, key, and role management, and every one of
  them has to reach the CLI and the API too (ADR 0004). That is a large amount
  of surface for what is conceptually a small model.

## Amendment: users are global

A user belonged to a tenant. They no longer do: `users.tenant_id` is gone,
`users.email` is globally unique, and `api_keys.tenant_id` names the tenant a
session resolves to. Migration `0004_users_are_global.sql`.

### What the original decision cost

The alternative above was rejected on the strength of one true observation —
ownership makes deletion a cascade — without weighing what ownership does
everywhere else:

- **Signing in required naming a tenant.** Not because authentication needed
  it, but because the email was only unique *within* one. Every person had to
  know a routing detail to identify themselves.
- **The same email in two tenants was two different people**, and they were
  indistinguishable at the login screen. A person whose account had been moved
  got exactly the response a wrong password gets, which is the worst possible
  behaviour for the one screen where a user cannot see any state.
- **A principal's catalog came from whichever tenant owned their row.** So an
  administrator's own gateway view depended on an arbitrary property of their
  user record, and seeding one in a tenant with no backend bindings produced an
  account that could administer everything and see nothing.
- **The seed had to maintain a decoy.** `dev-admin@mcpdoll.local` existed in
  `platform` and in `acme`, and one of them had to be deleted on every run,
  because leaving it meant the README could name a tenant that failed to log in.

None of these are deletion semantics. All of them are the daily path.

### What actually needed the tenant

One MCP session must resolve to exactly one tenant — tool names would otherwise
collide across tenants inside a single catalog, and ADR 0019 puts every tenant
behind one endpoint. That is a property of the **credential**, not of the
person: the key names the tenant it acts in. This is the shape ragdoll already
uses (`api_keys.environment_id`), and diverging from it was the error.

### What the reversal costs, and what pays for it

Deleting a tenant is now a query. Specifically it is three: cascade its keys,
delete every grant scoped beneath it, and leave the users alone. The middle one
is the part that matters and the part the original ADR was right to worry
about — a grant names its scope as *text*, so nothing cascades, and a grant left
behind is not merely dormant. A tenant recreated with the same slug would
silently re-authorize everyone granted into the old one. `Store.DeleteTenant`
does it explicitly and a test asserts the grant is gone.

So the cost is real, it is one function, and it is tested. The original decision
traded that for a login screen that could not tell a moved account from a wrong
password.
