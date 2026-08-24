# ADR 0017: Per-Tenant Backend Bindings, Pools, and a Declared Primary

## Status

**Accepted.** Extends [0005](./0005-content-addressed-tool-definitions.md)
(digests are now per tenant) and [0006](./0006-serve-admitted-not-observed.md)
(admission is per tenant).

## Context

The same logical backend is a different host per tenant. Tenant Acme's CRM is
`acme.realapp.com`; tenant Globex's is `globex.realapp.com`. Same vendor, same
toolset, different data — and, because they are separately deployed, eventually
different *versions*.

The old model had one `endpoint` per registered server and one admitted
definition per tool. Both assumptions fail here, and the second one fails in a
way that matters: if Acme is on CRM v2.1 and Globex on v1.9, they publish
different input schemas for `crm.lookup_customer`. There is no single correct
admitted definition.

A tenant may also run several interchangeable hosts — `a1.realapp.com`,
`a2.realapp.com` — for availability. During a rolling deploy those two disagree
with each other for a few minutes.

So there are two distinct kinds of divergence and they need different answers:

| | Divergence between | Meaning |
|---|---|---|
| **Across tenants** | Acme v2.1 vs Globex v1.9 | Normal. They are different deployments. |
| **Within a tenant's pool** | a1 v2.1 vs a2 v1.9 | Transient. They are supposed to be identical. |

## Decision

### A backend binding is `(toolset, tenant) → pool`

```yaml
backends:
  - toolset: ts_crm
    tenant: acme
    primary: https://a1.acme.realapp.com     # the definition source
    replicas:
      - https://a2.acme.realapp.com
  - toolset: ts_crm
    tenant: globex
    primary: https://globex.realapp.com
```

### Admission is per `(toolset, tenant)`

Each binding is discovered and admitted independently, with its own digests.
Acme's `lookup_customer` and Globex's are two admitted definitions that happen
to share a name. Drift for a tenant is measured against **that tenant's own**
admitted definition, never against another tenant's.

This is the direct consequence of cross-tenant divergence being normal. It also
means ADR 0005's content addressing now identifies `(tenant, tool)` rather than
`tool`, and the snapshot is partitioned by tenant.

### The primary is the definition source; replicas must match it

Discovery reads the **primary**. Replicas are probed and compared:

- **A replica matching the primary** is in the routable pool.
- **A replica whose semantic digest diverges** is removed from routing and
  flagged. It is not ejected from the config, and **the catalog does not
  change**.

```
tenant acme pool: a1 (primary, v2.1), a2 (v1.9)

a2 diverges  ->  removed from routing, shown in Backend health
                 catalog UNCHANGED, all traffic to a1
a2 upgrades  ->  matches primary  ->  rejoins
```

Capacity degrades; correctness does not. This is what a load balancer does with
an unhealthy member, and it reuses the prober and drift classifier already
built — a replica leaving the pool is `DriftSemantic` against the primary,
scored exactly as backend drift already is.

The alternative — admitting only what every member publishes identically —
would drop a tool from the catalog for the duration of every rolling deploy,
invalidating every client's prompt cache twice. ADR 0006's grace window exists
specifically to avoid that class of churn; taking it back here would be
inconsistent.

### Cosmetic divergence within a pool does not eject

A replica whose *description* differs but whose schema does not is left in the
pool. The gateway serves the primary's admitted description regardless (ADR
0006), so the difference is unobservable to a client, and ejecting a healthy
host over prose would trade real capacity for nothing.

### A binding with no reachable primary fails closed for that tenant

If a tenant's primary cannot be discovered at build time, that
`(toolset, tenant)` binding is not admitted — for **that tenant only**. Other
tenants publish normally.

Partial publication is correct here in a way it is not elsewhere: a snapshot
that omitted one tenant's toolset is a smaller catalog for that tenant, whereas
refusing the whole build would take an unrelated tenant's working configuration
hostage to a third party's outage. `--allow-unreachable` still governs whether
this is a warning or a build failure.

## Alternatives considered

- **One canonical definition per toolset across all tenants.** Rejected: it
  makes cross-tenant version skew an outage. A tenant on an older release would
  have its tools blocked as drifted, for having done nothing wrong.
- **Intersection admission within a pool** (only tools every member agrees on).
  Rejected above — it converts every rolling deploy into catalog churn.
- **No primary; any divergence ejects the odd host out.** Rejected because with
  a two-member pool there is no odd one out. A declared primary makes "which is
  correct" a configuration decision rather than a vote.
- **Per-tenant registry documents.** Rejected: the tool *classification* —
  which tools are destructive, which are excluded — is a property of the vendor's
  API, identical across tenants. Duplicating it per tenant means N places to
  update when a tool becomes destructive, and N chances to miss one.
- **Routing by tenant at request time against a shared backend list.** Rejected:
  it separates "which host" from "which definitions were admitted from that
  host", and those must not be able to disagree.

## Consequences

- **The snapshot grows with tenants × toolsets**, not with toolsets. This is
  accepted (see ADR 0018) and is the largest single scaling factor in the
  design.
- **Discovery cost is per binding.** A build with 200 tenants discovers 200
  primaries plus their replicas. Concurrency is bounded; a build is now
  measured in tens of seconds rather than in seconds, and that belongs in the
  publish workflow's expectations.
- **`Server` in the registry becomes two things**: the vendor-level
  classification (shared) and the per-tenant binding (not). Conflating them
  again would reintroduce the duplication rejected above.
- **A tenant can be silently smaller than intended** if its primary was
  unreachable at build time and `--allow-unreachable` was set. The build report
  and the console must both name which tenants were affected — a warning nobody
  reads is how this becomes an incident.
