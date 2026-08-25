# ADR 0028: Roles Are Composed, Not Compiled — and Granting One Is Bounded by What You Hold

## Status

**Accepted.** Opens the role half of
[0015](./0015-rbac-scopes-and-engines.md) while keeping the permission half
closed. Closes an escalation path that
[0022](./0022-control-plane-sessions-and-permissions.md) named but did not fully
seal, and fixes a permission that was computed and never enforced.

## Context

Roles were a Go function. `platform_admin`, `tenant_admin`, `tool_user` and four
others, each a fixed bundle, editable only by shipping a release.

The permission *vocabulary* is closed for a good reason: a permission nothing
enforces is a role that appears to grant something and does not, and a set that
grows casually stops being reviewable. That friction is worth keeping.

Composing roles out of that vocabulary is a different thing, and it is ordinary
administration. "Somebody who can publish but not edit the registry" is a
sentence an operator should be able to write on a Tuesday, not a pull request.

## Decision

**A role is a row: a name, a description, and a set of permissions drawn from
the closed vocabulary. The vocabulary stays closed.**

Built-in roles are seeded on every boot and cannot be deleted — a grant left
pointing at a deleted role authorizes nothing, and the seed would recreate it
anyway. Their permissions *are* editable: an operator who wants `viewer` to stop
seeing the registry is entitled to that.

### The rule that makes this safe is at grant time, not definition time

Two checks, and only one of them matters:

- **Defining** a role refuses permissions you do not hold *anywhere*. This is
  the weaker one, and it exists for the error message: it says "you cannot put
  `signingkey:generate` in a role" when somebody types it, rather than letting
  them build a role that is refused every time they try to use it.
- **Granting** a role refuses if it confers any permission you do not hold *at
  that scope*. This is the one that holds the line.

Without the second, user-defined roles would be a general escalation: define a
role carrying anything, grant it to yourself where you administer, hold it. The
difference between roles would be decoration — exactly what ADR 0022 refused
for the fixed catalog and then only half-enforced.

### The hole predates editable roles

`role:manage` was the only thing checked at grant time. It says you may
administer grants at a scope; it says nothing about *what* you may confer.

`tenant_admin` deliberately lacks `tenant:manage`. It holds `role:manage` in its
own tenant. So a tenant admin could always grant themselves `platform_admin` at
their own tenant and pick up `tenant:manage` there. Editable roles did not
create that; they made it general enough to notice.

### tool:call was computed and never enforced

Found by running the feature rather than reading it: a role with `tool:list` and
no `tool:call` listed eight tools and successfully called one.

The composed view had been computing a `callable` set since ADR 0015 and nothing
on the dispatch path read it — `Callable` had exactly one caller and it was a
unit test. It stayed invisible because every built-in role with `tool:list` also
had `tool:call`, with one exception: `viewer`, which could call anything it
could see.

The edge now refuses the call. This is a fix to shipped behaviour, not to
anything user-defined roles introduced.

### Declarative, like grants

The permissions sent are what the role holds afterwards. Expressing a role as a
sequence of deltas is how a permission gets left behind on one somebody thought
they had narrowed — the same argument `users grants set` already makes.

A role with no permissions is legal and useful: it neutralizes a role without
deleting it and breaking the grants that name it.

### Deleting is refused while anybody holds it

Not cascaded. A grant naming a deleted role authorizes nothing, and the person
who loses access is not the person running the command — they find out when
their agent stops.

## Alternatives considered

- **Leave roles compiled.** Honest, and it makes every role model change a
  release. Rejected: the friction was buying nothing, because the reviewable
  thing is the permission set and that stays closed either way.
- **Let roles nest (a role that includes another).** Expressive and a
  familiar model. Rejected for now: cycle detection, and "what does this role
  actually confer" stops being readable off one row, which is the property the
  grant-time check depends on being cheap.
- **Scope-bound roles — a role that only exists in one tenant.** Rejected: a
  role is a shape, the scope arrives when it is granted, and binding them would
  mean the same shape defined once per tenant and drifting.
- **Check only at definition time.** Simpler, one check. Rejected: it is the
  wrong one. Roles can be defined by an administrator and granted by somebody
  weaker, and the escalation happens at the grant.

## Consequences

- **Two escalation checks to keep in agreement.** They ask different questions
  at different scopes, and the weaker one is a courtesy. If they ever disagree
  the grant-time one is authoritative.
- **A role's permissions are editable, including built-ins'.** Narrowing
  `tool_user` changes what every agent holding it can do, immediately, at
  principal-publish latency. That is the point and it is sharp.
- **Descriptions are seeded but never overwritten.** A restart must not revert
  what somebody wrote — the same rule the schedules follow.
- **`viewer` can no longer call tools.** That is a behaviour change for any
  deployment relying on it, and it was a bug being relied upon.
