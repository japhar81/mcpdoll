import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";

import { getCatalog, mintAPIKey } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats, Table } from "../components/Screen.tsx";
import { useAuth } from "../lib/auth.tsx";
import { useInspection } from "../lib/inspection.tsx";

/** An agent credential is `mcpd.<prefix>.<secret>`. */
const AGENT_KEY = /^mcpd\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/;

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
  const auth = useAuth();
  const [applied, setApplied] = useState<string | null>(null);
  const [full, setFull] = useState(false);
  const [minted, setMinted] = useState(false);

  // Checked here rather than at the gateway. The deployment token and a session
  // are *control-plane* credentials — the gateway has never heard of them — so
  // presenting one gets a 403 that reads as "your key was rejected" when the
  // real answer is "that is not the kind of credential this endpoint takes".
  const wrongKind = credential.trim() !== "" && !AGENT_KEY.test(credential.trim());

  // Mint one for yourself: an agent credential carrying exactly your own
  // grants, so the catalog below is what you would be served.
  const mint = useMutation({
    mutationFn: () => {
      const expires = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      return mintAPIKey(auth.session!.user_id!, {
        name: `console inspection ${new Date().toISOString().slice(0, 19)}Z`,
        expires_at: expires,
      });
    },
    onSuccess: (result) => {
      setCredential(result.secret);
      setApplied(null);
      setMinted(true);
    },
  });

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
        {wrongKind && (
          <div className="note warn">
            <strong>That is not an agent key.</strong> This field takes a
            credential an <em>agent</em> presents to the gateway — it starts{" "}
            <code>mcpd.</code> The deployment token and your console session are
            control-plane credentials; the gateway has never heard of them and
            will refuse them.
            {auth.session?.user_id && (
              <>
                {" "}
                Mint one for yourself below.
              </>
            )}
          </div>
        )}

        {auth.session?.user_id && (
          <div className="row">
            <button
              className="secondary"
              disabled={mint.isPending}
              onClick={() => mint.mutate()}
            >
              {mint.isPending ? "Minting…" : "Inspect as me"}
            </button>
            <span className="muted">
              Mints a key carrying your own grants, valid for an hour.
            </span>
          </div>
        )}
        {mint.error != null && <ErrorBlock error={mint.error} />}
        {minted && (
          <p className="muted">
            Minted and filled in above. It expires in an hour; revoke it from
            your user page if you want it gone sooner.
          </p>
        )}

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
            disabled={!credential.trim() || wrongKind}
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
          Paste an agent&apos;s key and connect. A key whose owner holds no
          grants shows an empty catalog rather than an error.
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
            <strong>Every catalog is private.</strong> It is built from this
            principal&apos;s grants, so no two principals necessarily see the
            same list and no cache may share one between them.
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
