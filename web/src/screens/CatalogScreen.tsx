import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { getAudienceCatalog } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats, Table } from "../components/Screen.tsx";
import { IdentityFields } from "../components/IdentityFields.tsx";
import {
  toGroupList,
  useIdentity,
  type IdentityValue,
} from "../lib/identity.tsx";

/**
 * The tool list one identity actually receives.
 *
 * This is the answer to "which tools can this agent call?" that cannot be
 * wrong, because it is produced by making the same request the agent makes.
 * Re-deriving it from policy is how people get it wrong.
 */
export function CatalogScreen() {
  const { slug = "" } = useParams();
  const { identity } = useIdentity();
  // `applied` is what was actually sent. Typing must not re-fire the request on
  // every keystroke: each one opens an MCP session against the data plane.
  const [applied, setApplied] = useState<IdentityValue | null>(null);
  const [full, setFull] = useState(false);

  const q = useQuery({
    queryKey: ["catalog", slug, applied?.subject, applied?.groups, full],
    queryFn: () =>
      getAudienceCatalog(
        slug,
        {
          subject: applied?.subject,
          groups: toGroupList(applied?.groups ?? ""),
        },
        full,
      ),
    enabled: applied !== null,
    retry: false,
  });

  return (
    <Screen
      title={`Catalog — ${slug}`}
      actions={
        <>
          <Link className="link" to={`/gateway/audiences/${slug}/playground`}>
            call a tool →
          </Link>
          <Link className="link" to="/gateway/audiences">
            all audiences
          </Link>
        </>
      }
    >
      <div className="card">
        <IdentityFields />
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
            onClick={() => setApplied({ ...identity })}
          >
            {q.isFetching ? "Connecting…" : "Connect and list"}
          </button>
        </div>
        <p className="muted">
          Connects to <code>/mcp/{slug}</code> as a real MCP client presenting
          this identity, and shows exactly what comes back.
        </p>
      </div>

      {applied === null && (
        <p className="muted">
          Set an identity and connect. An empty subject is a legitimate thing to
          try — it shows what an unidentified caller receives.
        </p>
      )}

      {q.error != null && <ErrorBlock error={q.error} />}

      {q.data && (
        <>
          <Stats
            items={[
              { k: "Tools", v: q.data.tools.length },
              { k: "TTL (ms)", v: q.data.ttl_ms },
              {
                k: "Cache scope",
                v:
                  q.data.cache_scope === "public" ? (
                    <span className="badge badge-ok">public</span>
                  ) : (
                    <span className="badge">{q.data.cache_scope}</span>
                  ),
              },
              { k: "Protocol", v: q.data.protocol_version, small: true },
              { k: "Server", v: q.data.server_name, small: true },
            ]}
          />

          {q.data.cache_scope === "public" &&
            (applied?.subject || applied?.groups) && (
              <div className="note warn">
                <strong>
                  This catalog was requested for a specific principal and came
                  back <code>public</code>.
                </strong>{" "}
                If anything filtered it, that is a cross-tenant cache leak.
                Check whether an identity-dependent plugin is enabled for this
                audience.
              </div>
            )}

          <Table
            columns={["Tool", "Namespace", "Description"]}
            rows={q.data.tools.map((t) => [
              <code>{t.name}</code>,
              t.namespace,
              <span className="muted">{t.description ?? ""}</span>,
            ])}
            empty="This identity receives no tools at all."
          />
        </>
      )}
    </Screen>
  );
}
