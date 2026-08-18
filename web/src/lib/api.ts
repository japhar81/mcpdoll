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
  AudienceList,
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

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {};
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
}

export const buildSnapshot = (options: BuildOptions) =>
  request<BuildReport>("POST", "/api/v1/snapshots:build", options);

export const generateSigningKey = (keyId: string) =>
  request<SigningKey>("POST", "/api/v1/keys:generate", { key_id: keyId });

// ----------------------------------------------------------------- gateway ---

export const getGatewayStatus = () =>
  request<GatewayStatus>("GET", "/api/v1/gateway/status");

export const listAudiences = () =>
  request<AudienceList>("GET", "/api/v1/gateway/audiences");

export interface Identity {
  subject?: string;
  groups?: string[];
}

export const getAudienceCatalog = (
  slug: string,
  identity: Identity,
  full = false,
) =>
  request<Catalog>(
    "GET",
    `/api/v1/gateway/audiences/${encodeURIComponent(slug)}/catalog` +
      query({
        subject: identity.subject,
        groups: identity.groups?.join(","),
        full,
      }),
  );

export interface CallToolInput extends Identity {
  arguments?: Record<string, unknown>;
  request_state?: string;
  /** Keyed by input-request id: accept | decline | cancel | text:<value> */
  responses?: Record<string, string>;
}

export const callTool = (
  slug: string,
  toolName: string,
  input: CallToolInput,
) =>
  request<CallResult>(
    "POST",
    `/api/v1/gateway/audiences/${encodeURIComponent(slug)}` +
      `/tools/${encodeURIComponent(toolName)}:call`,
    input,
  );
