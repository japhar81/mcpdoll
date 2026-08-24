import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";

import { getCatalog } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats, Table } from "../components/Screen.tsx";
import { useInspection } from "../lib/inspection.tsx";

/**
 * The tool list one credential actually receives.
 *
 * This is the answer to "which tools can this agent call?" that cannot be
 * wrong, because it is produced by presenting what the agent presents. There is
 * no audience to pick and no subject to claim: with one endpoint and
 * per-principal catalogs (ADR 0019), re-deriving what someone *should* see is
 * exactly the mistake this screen exists to avoid.
 */
export function CatalogScreen() {
  const { credential, setCredential } = useInspection();
  const [applied, setApplied] = useState<string | null>(null);
  const [full, setFull] = useState(false);

  const q = useQuery({
    queryKey: ["catalog", applied, full],
    queryFn: () => getCatalog(applied ?? "", full),
    enabled: applied !== null && applied !== "",
    retry: false,
  });

  return (
    <Screen
      title="Inspect a principal"
      actions={
        <Link className="link" to="/gateway/playground">
          call a tool →
        </Link>
      }
    >
      <div className="card">
        <label className="field">
          API key to inspect as
          <input
            type="password"
            spellCheck={false}
            placeholder="mcpd.…"
            value={credential}
            onChange={(e) => setCredential(e.target.value)}
          />
        </label>
        <div className="row">
          <label className="inline">
            <input
              type="checkbox"
              checked={full}
              onChange={(e) => setFull(e.target.checked)}
            />
            full descriptions
          </label>
          <span className="spacer" />
          <button
            className="primary"
            disabled={!credential.trim()}
            onClick={() => setApplied(credential.trim())}
          >
            {q.isFetching ? "Connecting…" : "Connect and list"}
          </button>
        </div>
        <p className="muted">
          Connects to <code>/mcp</code> as a real MCP client presenting this
          credential, and shows exactly what comes back. The tenant and the
          toolset both come from the key.
        </p>
      </div>

      {applied === null && (
        <p className="muted">
          Paste an agent&apos;s key and connect. A key with no grants is a
          legitimate thing to try — it shows an empty catalog, which is the
          correct state for a user nobody has granted anything yet.
        </p>
      )}

      {q.error != null && <ErrorBlock error={q.error} />}

      {q.data && (
        <>
          <Stats
            items={[
              { k: "Tenant", v: q.data.tenant, small: true },
              { k: "Subject", v: q.data.subject ?? "—", small: true },
              { k: "Tools", v: q.data.tools.length },
              { k: "TTL (ms)", v: q.data.ttl_ms },
              { k: "Cache scope", v: <span className="badge">{q.data.cache_scope}</span> },
              { k: "Protocol", v: q.data.protocol_version, small: true },
            ]}
          />

          <div className="note">
            <strong>Every catalog is private.</strong> It is derived from this
            principal&apos;s grants, so no two principals necessarily see the
            same list and none of it may be shared from a common cache. That is
            the permanent cost of per-user access control (ADR 0016).
          </div>

          <Table
            columns={["Tool", "Namespace", "Description"]}
            rows={q.data.tools.map((t) => [
              <code>{t.name}</code>,
              t.namespace,
              <span className="muted">{t.description ?? ""}</span>,
            ])}
            empty="This credential receives no tools at all — it holds no grants."
          />
        </>
      )}
    </Screen>
  );
}
