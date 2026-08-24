/**
 * Wire types, mirroring internal/api/types.go.
 *
 * Hand-written rather than generated, and that is a real cost: two definitions
 * of one shape can drift. The Go side is single-sourced (the server and the CLI
 * marshal the same structs), so the drift risk is confined to this file, and
 * `make parity` catches an operation that loses a surface. Generating these
 * from api/openapi.yaml is the right end state — see docs/deferred.md.
 */

export interface Health {
  status: string;
  version: string;
  registry_path?: string;
  snapshot_path?: string;
}

export interface HookList {
  hooks: string[];
}

export interface Namespace {
  id: string;
  name: string;
  prefix: string;
  owner_idp_group?: string;
  team?: string;
  project?: string;
}

export interface Server {
  id: string;
  name: string;
  namespace: string;
  bindings: Binding[];
  /** Resolved, never empty: an unset serving_mode means "strict". */
  serving_mode: string;
  criticality?: string;
  data_classification?: string;
  compliance_scope?: string[];
  default_effect_class: string;
  canary_tool?: string;
  /** Tools whose classification the registry states explicitly. */
  tool_overrides?: Record<string, string>;
  /** Withheld from every audience. Not an override with an empty class. */
  excluded_tools?: string[];
}

/** A named, grantable group of tools. Its name appears in every grant scope. */
export interface Toolset {
  id: string;
  name: string;
  priority: number;
  token_budget?: number;
  namespaces: string[];
  tools?: string[];
  exclude?: string[];
}

/** One tenant's hosts for a backend. */
export interface Binding {
  tenant: string;
  /** The definition source; replicas are compared against it. */
  primary: string;
  replicas?: string[];
}

export interface Policy {
  id: string;
  name: string;
  priority: number;
  rule_count: number;
}

export interface Plugin {
  id: string;
  name: string;
  version?: string;
  runtime: string;
  hooks: string[];
  priority: number;
  /** Resolved: an unset rollout means "shadow". */
  rollout: string;
  canary_percent?: number;
  reads?: string[];
  writes?: string[];
  /** Forces cacheScope: private at on_catalog. */
  identity_dependent?: boolean;
  artifact_digest?: string;
}

export interface Registry {
  org: string;
  version: number;
  namespaces: Namespace[];
  servers: Server[];
  toolsets: Toolset[];
  policies?: Policy[];
  plugins?: Plugin[];
}

export interface ServerList {
  servers: Server[];
}

export interface PluginList {
  plugins: Plugin[];
}

export interface RegistrySummary {
  file?: string;
  valid: boolean;
  org: string;
  version: number;
  namespaces: number;
  servers: number;
  toolsets: number;
  policies: number;
  plugins: number;
}

export interface GatewayStatus {
  gateway_url: string;
  status: string;
  ready: boolean;
  snapshot_version: number;
  tenants: number;
  tools: number;
}

/**
 * One tenant, joined across the three places a tenant exists.
 *
 * `users: 0` with `backends > 0` is a tenant nothing can authenticate into;
 * `backends: 0` is a tenant no tool can reach whatever its users are granted.
 */
export interface TenantSummary {
  /** Absent for a slug that appears only in the registry. */
  id?: string;
  slug: string;
  name: string;
  /** The tenant's own status, or `unregistered` for a registry-only slug. */
  status: string;
  users: number;
  /** Registry bindings naming this tenant. */
  backends: number;
  /** Tools the serving snapshot admits for this tenant. */
  tools: number;
  created_at?: string;
}

export interface TenantList extends GatewayStatus {
  /** Joined across the database, the registry, and the serving snapshot. */
  registered: TenantSummary[];
}

export interface Tenant {
  id: string;
  /** Appears verbatim in every scope string, and is immutable for that reason. */
  slug: string;
  name: string;
  status: string;
  created_at: string;
}

export interface User {
  id: string;
  tenant_id: string;
  /** The slug, because every scope this user appears in is written with it. */
  tenant: string;
  email: string;
  display_name?: string;
  status: string;
  /** Whether local sign-in is possible. Never the hash. */
  has_password: boolean;
  created_at: string;
}

export interface UserList {
  tenant: string;
  users: User[];
}

/** One role held at one scope. Scopes nest: `*` ⊃ `t/x` ⊃ `t/x/ts/y` ⊃ `t/x/ts/y/z`. */
export interface Grant {
  role: string;
  scope: string;
}

export interface GrantList {
  user_id: string;
  grants: Grant[];
}

export interface APIKey {
  id: string;
  user_id: string;
  name: string;
  /** The lookup half, public by construction. */
  prefix: string;
  /** What the key asks for; effective grants are this ∩ the owner's. */
  declared_grants: Grant[];
  /** Whether it would authenticate right now. */
  active: boolean;
  created_at: string;
  last_used_at?: string;
  expires_at?: string;
  revoked_at?: string;
}

export interface APIKeyList {
  user_id: string;
  keys: APIKey[];
}

/** A new key. The one time its secret is knowable. */
export interface MintedAPIKey {
  key: APIKey;
  secret: string;
}

export interface Role {
  name: string;
  permissions: string[];
}

export interface RoleCatalog {
  roles: Role[];
  /** Every permission that exists, not only the ones some role uses. */
  permissions: string[];
}

export interface CatalogTool {
  name: string;
  namespace: string;
  title?: string;
  description?: string;
}

export interface Catalog {
  /** There is no audience: the principal is the audience (ADR 0016). */
  tenant: string;
  subject?: string;
  protocol_version: string;
  server_name: string;
  /** A filtered catalog that came back `public` is a cross-tenant leak. */
  ttl_ms: number;
  cache_scope: string;
  tools: CatalogTool[];
}

export interface CallResult {
  tool: string;
  is_error: boolean;
  /** An MRTR deferral: the call did not fail, it is waiting for a human. */
  needs_input: boolean;
  duration_ms: number;
  text: string;
  gateway_detail?: unknown;
  input_requests?: string[];
  /** Unforgeable, not secret. Echo it back with the answers. */
  request_state?: string;
}

/** One tenant's slice of a snapshot. Each principal sees a subset of it. */
export interface TenantSnapshotSummary {
  slug: string;
  name: string;
  tools: number;
  token_estimate: number;
}

export interface ToolSummary {
  qualified_name: string;
  backend: string;
  effect_class: string;
  token_estimate: number;
  digest: string;
}

export interface Snapshot {
  source?: string;
  version: number;
  snapshot_id: string;
  org: string;
  built_at: string;
  age: string;
  key_id: string;
  algorithm: string;
  registry_digest: string;
  /** Whether it would actually activate. Signature validity is a separate question. */
  servable: boolean;
  unservable_reason?: string;
  tenants: TenantSnapshotSummary[];
  tools?: ToolSummary[];
}

export interface BackendReport {
  server_id: string;
  server_name: string;
  endpoint: string;
  negotiated_version?: string;
  tool_count: number;
  admitted: string[];
  excluded?: string[];
  observed_at: string;
}

export interface BuildReport {
  version: number;
  snapshot_id: string;
  org: string;
  registry_digest: string;
  key_id: string;
  public_key: string;
  namespaces: number;
  servers: number;
  tools: number;
  toolsets: number;
  plugins: number;
  backends: BackendReport[];
  warnings?: string[];
  output?: string;
  dry_run: boolean;
}

export interface VerifyReport {
  source?: string;
  valid: boolean;
  version: number;
  key_id: string;
  tenants: string[];
  tools: number;
}

/** The public half only. The private key never crosses the network. */
export interface SigningKey {
  key_id: string;
  directory: string;
  public_key: string;
  trust_entry: string;
}

export interface ToolDrift {
  name: string;
  /** Absent for an added tool — assigning a qualified name is admission's job. */
  qualified_name?: string;
  kind: "cosmetic" | "semantic" | "removed" | "added";
  admitted_digest?: string;
  observed_digest?: string;
  detail: string;
}

export interface BackendHealth {
  server_id: string;
  server_name: string;
  endpoint: string;
  /** unknown is distinct from healthy: it means nobody has looked yet. */
  state: "unknown" | "healthy" | "degraded" | "unavailable" | "drifted";
  serving_mode: string;
  last_probe: string;
  last_success?: string;
  consecutive_failures: number;
  latency_ewma_ms: number;
  negotiated_version?: string;
  error?: string;
  tools_admitted: number;
  tools_observed: number;
  drift?: ToolDrift[];
}

export interface BackendHealthSummary {
  total: number;
  healthy: number;
  degraded: number;
  unavailable: number;
  drifted: number;
  unknown: number;
  /** Always zero in an all-advisory deployment, however far backends moved. */
  blocked_tools: number;
}

export interface BackendHealthReport {
  summary: BackendHealthSummary;
  backends: BackendHealth[];
}

export interface ApiErrorBody {
  code: string;
  message: string;
  problems?: string[];
}
