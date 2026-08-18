import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, getRegistry, listPlugins, setToken, validateRegistry } from "./api.ts";

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

    expect(calls[0]!.init.headers).toMatchObject({ Authorization: "Bearer s3cret" });
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
          problems: ["catalog.ttl must be positive", "audience references unknown bundle"],
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
    stubFetch(() => new Response("<html>502 Bad Gateway</html>", { status: 502 }));

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

  it("encode a slug that contains a slash", async () => {
    const calls = stubFetch(() => json({ audience: "x", tools: [] }));
    const { getAudienceCatalog } = await import("./api.ts");
    await getAudienceCatalog("a/b", { subject: "alice@example.com" });

    expect(calls[0]!.url).toContain("/audiences/a%2Fb/catalog");
    expect(calls[0]!.url).toContain("subject=alice%40example.com");
  });
});
