import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ApiError,
  getRegistry,
  listPlugins,
  setToken,
  validateRegistry,
} from "./api.ts";

/** Installs a fetch stub and returns the recorded calls. */
function stubFetch(handler: (url: string, init: RequestInit) => Response) {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  vi.stubGlobal("fetch", (url: string, init: RequestInit) => {
    calls.push({ url, init });
    return Promise.resolve(handler(url, init));
  });
  return calls;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  setToken("");
});

describe("the token", () => {
  it("is sent as a bearer credential", async () => {
    const calls = stubFetch(() => json({ org: "o", version: 1 }));
    setToken("s3cret");
    await getRegistry();

    expect(calls[0]!.init.headers).toMatchObject({
      Authorization: "Bearer s3cret",
    });
  });

  it("is omitted entirely when unset, rather than sent empty", async () => {
    // `Authorization: Bearer ` is a malformed header that a server has to
    // decide about. Sending nothing is unambiguous.
    const calls = stubFetch(() => json({ hooks: [] }));
    await listPlugins();

    expect(calls[0]!.init.headers).not.toHaveProperty("Authorization");
  });
});

describe("errors", () => {
  it("carry every problem, not just the message", async () => {
    stubFetch(() =>
      json(
        {
          code: "validation_failed",
          message: "the registry document is not valid",
          problems: [
            "catalog.ttl must be positive",
            "audience references unknown bundle",
          ],
        },
        422,
      ),
    );

    // A console that renders one line of a six-problem validation makes the
    // reader run it six times.
    const err = await validateRegistry("org: x").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(422);
    expect((err as ApiError).code).toBe("validation_failed");
    expect((err as ApiError).problems).toHaveLength(2);
  });

  it("distinguish an unreachable control plane from an HTTP failure", async () => {
    vi.stubGlobal("fetch", () => Promise.reject(new Error("ECONNREFUSED")));

    const err = (await getRegistry().catch((e: unknown) => e)) as ApiError;
    // Status 0 rather than 500: reporting a status would claim the control
    // plane answered, which sends the reader to the wrong logs.
    expect(err.status).toBe(0);
    expect(err.message).toContain("cannot reach the control plane");
  });

  it("survive a non-JSON body", async () => {
    // A proxy's HTML error page is a real thing to receive, and it must not
    // turn into a parse exception with no status in it.
    stubFetch(
      () => new Response("<html>502 Bad Gateway</html>", { status: 502 }),
    );

    const err = (await getRegistry().catch((e: unknown) => e)) as ApiError;
    expect(err.status).toBe(502);
    expect(err.message).toContain("502");
  });
});

describe("query strings", () => {
  it("omit false and empty values rather than sending them", async () => {
    const calls = stubFetch(() => json({ version: 1, audiences: [] }));
    const { getCurrentSnapshot } = await import("./api.ts");
    await getCurrentSnapshot(false);

    // `?tools=false` reads as a deliberate choice to a server that only checks
    // for the parameter's presence.
    expect(calls[0]!.url).toBe("/api/v1/snapshots/current");
  });

  it("encode a tool name that contains a slash", async () => {
    const calls = stubFetch(() => json({ audience: "x", tools: [] }));
    const { callTool } = await import("./api.ts");
    await callTool("crm.lookup/weird", { credential: "mcpd.a.b" });

    // A tool name is operator-supplied and reaches a URL path; without
    // encoding, one containing a slash would address a different route.
    expect(calls[0]!.url).toContain("/tools/crm.lookup%2Fweird:call");
  });
});

describe("the tenancy operations", () => {
  it("send the whole grant set, so an omission is a revocation", async () => {
    const calls = stubFetch(() => json({ user_id: "u1", grants: [] }));
    const { putGrants } = await import("./api.ts");

    await putGrants("u1", [{ role: "tool_user", scope: "t/acme" }]);

    expect(calls[0]!.init.method).toBe("PUT");
    expect(JSON.parse(calls[0]!.init.body as string)).toEqual({
      grants: [{ role: "tool_user", scope: "t/acme" }],
    });
  });

  it("send an empty set rather than skipping the request", async () => {
    // Stripping an account without deleting it is a real operation. A client
    // that treated "no grants" as "nothing to do" would make it impossible.
    const calls = stubFetch(() => json({ user_id: "u1", grants: [] }));
    const { putGrants } = await import("./api.ts");

    await putGrants("u1", []);

    expect(JSON.parse(calls[0]!.init.body as string)).toEqual({ grants: [] });
  });

  it("omit an absent expiry rather than sending an empty string", async () => {
    // `expires_at: ""` is not RFC 3339 and the server refuses it. A key with no
    // expiry has to send no field at all.
    const calls = stubFetch(() =>
      json({ key: { id: "k1" }, secret: "mcpd.a.b" }, 201),
    );
    const { mintAPIKey } = await import("./api.ts");

    await mintAPIKey("u1", { name: "bot" });

    expect(JSON.parse(calls[0]!.init.body as string)).toEqual({ name: "bot" });
  });

  it("encode a user id into the path", async () => {
    const calls = stubFetch(() => new Response(null, { status: 204 }));
    const { revokeAPIKey } = await import("./api.ts");

    await revokeAPIKey("a/b");

    expect(calls[0]!.url).toBe("/api/v1/keys/a%2Fb");
  });

  it("surface a 409 as a conflict a caller can act on", async () => {
    stubFetch(() =>
      json({ code: "invalid_request", message: "slug acme already exists" }, 409),
    );
    const { createTenant } = await import("./api.ts");

    await expect(createTenant("acme", "Acme")).rejects.toMatchObject({
      status: 409,
      message: "slug acme already exists",
    });
  });

  it("report an absent database as its own thing, not as an empty list", async () => {
    stubFetch(() =>
      json(
        {
          code: "upstream_unavailable",
          message: "this control plane has no database configured",
        },
        503,
      ),
    );
    const { listUsers } = await import("./api.ts");

    await expect(listUsers("t1")).rejects.toBeInstanceOf(ApiError);
  });
});
