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
  endpoint: string;
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

export interface Bundle {
  id: string;
  name: string;
  priority: number;
  token_budget?: number;
  namespaces: string[];
}

export interface Audience {
  id: string;
  slug: string;
  name?: string;
  bundles: string[];
  policies?: string[];
  allowed_idp_groups?: string[];
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
  bundles: Bundle[];
  audiences: Audience[];
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
  bundles: number;
  audiences: number;
  policies: number;
  plugins: number;
}

export interface GatewayStatus {
  gateway_url: string;
  status: string;
  ready: boolean;
  snapshot_version: number;
  audiences: number;
}

export interface AudienceList extends GatewayStatus {
  /** What the registry declares — not necessarily what is being served now. */
  registered: Audience[];
}

export interface CatalogTool {
  name: string;
  namespace: string;
  title?: string;
  description?: string;
}

export interface Catalog {
  audience: string;
  subject?: string;
  groups?: string[];
  protocol_version: string;
  server_name: string;
  /** A filtered catalog that came back `public` is a cross-tenant leak. */
  ttl_ms: number;
  cache_scope: string;
  tools: CatalogTool[];
}

export interface CallResult {
  tool: string;
  audience: string;
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

export interface AudienceSummary {
  slug: string;
  name: string;
  tools: number;
  ttl_ms: number;
  cache_scope: string;
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
  audiences: AudienceSummary[];
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
  bundles: number;
  audiences: number;
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
  audiences: string[];
  tools: number;
}

/** The public half only. The private key never crosses the network. */
export interface SigningKey {
  key_id: string;
  directory: string;
  public_key: string;
  trust_entry: string;
}

export interface ApiErrorBody {
  code: string;
  message: string;
  problems?: string[];
}
