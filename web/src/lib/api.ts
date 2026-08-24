/**
 * Typed client over the control-plane API (api/openapi.yaml).
 *
 * One function per operationId, named for it. That is not decoration: the
 * parity check keys on operationIds, and a client whose functions are named
 * after the operations makes a rename visible here as a compile error rather
 * than as a 404 nobody notices until a user hits it.
 *
 * Pure module — no React, so it is usable from a script or a test.
 */
import type {
  APIKeyList,
  Grant,
  GrantList,
  MintedAPIKey,
  RevocationReport,
  RoleCatalog,
  Session,
  SessionInfo,
  Tenant,
  TenantList,
  User,
  UserList,
  BackendHealthReport,
  BuildReport,
  CallResult,
  Catalog,
  GatewayStatus,
  Health,
  HookList,
  ApiErrorBody,
  PluginList,
  Registry,
  RegistrySummary,
  Server,
  ServerList,
  SigningKey,
  Snapshot,
  VerifyReport,
} from "./types.ts";

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  /** Every distinct problem. Validation returns all of them at once. */
  readonly problems: string[];

  constructor(
    status: number,
    body: ApiErrorBody | undefined,
    fallback: string,
  ) {
    super(body?.message ?? fallback);
    this.name = "ApiError";
    this.status = status;
    this.code = body?.code ?? "unknown";
    this.problems = body?.problems ?? [];
  }
}

let token = "";

/** Set the bearer token sent with every subsequent request. */
export function setToken(next: string): void {
  token = next;
}

export function getToken(): string {
  return token;
}

type UnauthorizedHandler = () => void;

let onUnauthorized: UnauthorizedHandler | null = null;

/**
 * Register what to do when any request comes back 401.
 *
 * A credential can stop working mid-session — the control plane restarts with
 * a different token, or an operator rotates it. Without this, every open screen
 * would render its own "unauthorized" error and the user would have to work out
 * that they need to sign in again.
 */
export function setUnauthorizedHandler(
  handler: UnauthorizedHandler | null,
): void {
  onUnauthorized = handler;
}

/**
 * Check whether a credential is accepted, without adopting it.
 *
 * It asks for the hook list, which is the cheapest authenticated operation
 * there is: a static array of seven strings, behind the same auth wall as
 * everything else, touching no file and no backend. Verifying against
 * something expensive would make a wrong password cost a registry read.
 *
 * Deliberately does not go through `request`, for two reasons: the token under
 * test must not be installed before it is known to work, and a failed check
 * must not trip the global unauthorized handler — the caller is *asking* about
 * a 401, not suffering one.
 */
export async function probeCredential(
  candidate: string,
): Promise<"ok" | "unauthorized" | "unreachable"> {
  const headers: Record<string, string> = {};
  if (candidate) headers["Authorization"] = `Bearer ${candidate}`;

  try {
    const response = await fetch("/api/v1/hooks", { method: "GET", headers });
    if (response.ok) return "ok";
    if (response.status === 401 || response.status === 403)
      return "unauthorized";
    // Any other status means the control plane answered and something else is
    // wrong. Reporting that as a bad password would send the user to fix the
    // one thing that is fine.
    return "unreachable";
  } catch {
    return "unreachable";
  }
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  extraHeaders?: Record<string, string>,
): Promise<T> {
  const headers: Record<string, string> = { ...extraHeaders };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";

  let response: Response;
  try {
    response = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (cause) {
    // A network failure is not an HTTP status. Reporting it as one would
    // suggest the control plane answered, which is the opposite of what
    // happened and sends the reader to the wrong logs.
    throw new ApiError(
      0,
      undefined,
      `cannot reach the control plane: ${String(cause)}`,
    );
  }

  if (response.status === 204) return undefined as T;

  const text = await response.text();
  let parsed: unknown;
  try {
    parsed = text ? JSON.parse(text) : undefined;
  } catch {
    parsed = undefined;
  }

  if (!response.ok) {
    if (response.status === 401) {
      // Told once, centrally. Every screen rendering its own 401 leaves the
      // user to deduce that the fix is signing in again.
      onUnauthorized?.();
    }
    throw new ApiError(
      response.status,
      parsed as ApiErrorBody | undefined,
      `${method} ${path} failed with ${response.status}`,
    );
  }
  return parsed as T;
}

function query(params: Record<string, string | boolean | undefined>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === "" || value === false) continue;
    search.set(key, String(value));
  }
  const qs = search.toString();
  return qs ? `?${qs}` : "";
}

// ------------------------------------------------------------------ system ---

export const getHealth = () => request<Health>("GET", "/healthz");

export const listHooks = () => request<HookList>("GET", "/api/v1/hooks");

// ---------------------------------------------------------------- registry ---

export const getRegistry = () => request<Registry>("GET", "/api/v1/registry");

export const validateRegistry = (content: string) =>
  request<RegistrySummary>("POST", "/api/v1/registry:validate", { content });

export const listServers = () =>
  request<ServerList>("GET", "/api/v1/registry/servers");

export const getServer = (serverId: string) =>
  request<Server>(
    "GET",
    `/api/v1/registry/servers/${encodeURIComponent(serverId)}`,
  );

export const listPlugins = () => request<PluginList>("GET", "/api/v1/plugins");

// --------------------------------------------------------------- snapshots ---

export const getCurrentSnapshot = (tools = false) =>
  request<Snapshot>("GET", `/api/v1/snapshots/current${query({ tools })}`);

export const inspectSnapshot = (content: string, tools = false) =>
  request<Snapshot>("POST", "/api/v1/snapshots:inspect", { content, tools });

export const verifySnapshot = (content: string, trustedKeys: string[]) =>
  request<VerifyReport>("POST", "/api/v1/snapshots:verify", {
    content,
    trusted_keys: trustedKeys,
  });

export interface BuildOptions {
  allow_unreachable?: boolean;
  dry_run?: boolean;
  discover_timeout_ms?: number;
  concurrency?: number;
  /** Absent means a Unix timestamp — monotonic without coordination. */
  version?: number;
}

export const buildSnapshot = (options: BuildOptions) =>
  request<BuildReport>("POST", "/api/v1/snapshots:build", options);

export const generateSigningKey = (keyId: string) =>
  request<SigningKey>("POST", "/api/v1/keys:generate", { key_id: keyId });

// ----------------------------------------------------------------- gateway ---

export const getGatewayStatus = () =>
  request<GatewayStatus>("GET", "/api/v1/gateway/status");

export const listBackends = () =>
  request<BackendHealthReport>("GET", "/api/v1/gateway/backends");

export const listTenants = () => request<TenantList>("GET", "/api/v1/tenants");

/**
 * What one credential can see.
 *
 * A credential, not an audience and a subject: with one endpoint and
 * per-principal catalogs, the only trustworthy way to see what a principal sees
 * is to present what they present (ADR 0019).
 */
export const getCatalog = (credential: string, full = false) =>
  request<Catalog>("GET", "/api/v1/gateway/catalog" + query({ full }), undefined, {
    "X-MCPDoll-Inspect-Credential": credential,
  });

export interface CallToolInput {
  /** The API key to act as. */
  credential: string;
  arguments?: Record<string, unknown>;
  request_state?: string;
  /** Keyed by input-request id: accept | decline | cancel | text:<value> */
  responses?: Record<string, string>;
}

/**
 * Call one tool as one credential.
 *
 * No slug in the path: the tenant and the toolset are properties of the key,
 * not of the URL. Naming them separately would let the two disagree.
 */
export const callTool = (toolName: string, input: CallToolInput) =>
  request<CallResult>(
    "POST",
    `/api/v1/gateway/tools/${encodeURIComponent(toolName)}:call`,
    input,
  );

// ---------------------------------------------------------------- tenancy ---

export const createTenant = (slug: string, name: string) =>
  request<Tenant>("POST", "/api/v1/tenants", { slug, name });

export const deleteTenant = (tenantId: string) =>
  request<void>("DELETE", `/api/v1/tenants/${encodeURIComponent(tenantId)}`);

export const listUsers = (tenantId: string) =>
  request<UserList>(
    "GET",
    `/api/v1/tenants/${encodeURIComponent(tenantId)}/users`,
  );

export interface CreateUserInput {
  email: string;
  display_name?: string;
  /** Optional: an SSO user has none, and a key-only identity does not need one. */
  password?: string;
}

export const createUser = (tenantId: string, input: CreateUserInput) =>
  request<User>(
    "POST",
    `/api/v1/tenants/${encodeURIComponent(tenantId)}/users`,
    input,
  );

export const getUser = (userId: string) =>
  request<User>("GET", `/api/v1/users/${encodeURIComponent(userId)}`);

export const updateUser = (
  userId: string,
  input: { display_name?: string; status: string },
) => request<User>("PATCH", `/api/v1/users/${encodeURIComponent(userId)}`, input);

export const listGrants = (userId: string) =>
  request<GrantList>("GET", `/api/v1/users/${encodeURIComponent(userId)}/grants`);

/** The complete set the user should hold. Anything absent is revoked. */
export const putGrants = (userId: string, grants: Grant[]) =>
  request<GrantList>(
    "PUT",
    `/api/v1/users/${encodeURIComponent(userId)}/grants`,
    { grants },
  );

export const listAPIKeys = (userId: string) =>
  request<APIKeyList>("GET", `/api/v1/users/${encodeURIComponent(userId)}/keys`);

export interface MintAPIKeyInput {
  name: string;
  grants?: Grant[];
  /** RFC 3339. Absent means the key does not expire. */
  expires_at?: string;
}

export const mintAPIKey = (userId: string, input: MintAPIKeyInput) =>
  request<MintedAPIKey>(
    "POST",
    `/api/v1/users/${encodeURIComponent(userId)}/keys`,
    input,
  );

export const revokeAPIKey = (keyId: string) =>
  request<void>("DELETE", `/api/v1/keys/${encodeURIComponent(keyId)}`);

export const listRoles = () => request<RoleCatalog>("GET", "/api/v1/roles");

// ------------------------------------------------------------------- auth ---

/**
 * Sign in as a person.
 *
 * Deliberately not through `request`: the token under test must not be
 * installed before it is known to work, and a failed sign-in must not trip the
 * global unauthorized handler — the caller is *asking* about a 401, not
 * suffering one.
 */
export async function login(
  tenant: string,
  email: string,
  password: string,
): Promise<Session> {
  const response = await fetch("/api/v1/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant, email, password }),
  });
  const text = await response.text();
  let parsed: unknown;
  try {
    parsed = text ? JSON.parse(text) : undefined;
  } catch {
    parsed = undefined;
  }
  if (!response.ok) {
    throw new ApiError(
      response.status,
      parsed as ApiErrorBody | undefined,
      "sign-in failed",
    );
  }
  return parsed as Session;
}

export const getSession = () =>
  request<SessionInfo>("GET", "/api/v1/auth/session");

export const logout = () => request<void>("DELETE", "/api/v1/auth/session");

export const getRevocations = () =>
  request<RevocationReport>("GET", "/api/v1/revocations");
