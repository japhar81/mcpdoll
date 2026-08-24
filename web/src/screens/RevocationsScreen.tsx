import { useQuery } from "@tanstack/react-query";

import { getRevocations } from "../lib/api.ts";
import { Screen, Stats, Table } from "../components/Screen.tsx";

/**
 * What has been revoked, and whether the gateway has caught up.
 *
 * Both halves, because the gap between them is the exposure. ADR 0023 does not
 * eliminate the leaked-credential window — failing closed on an unreachable
 * list would let a control-plane outage stop tool calls, reversing the property
 * the whole architecture provides. It bounds it, and this screen is the bound.
 */
export function RevocationsScreen() {
  const q = useQuery({
    queryKey: ["revocations"],
    queryFn: getRevocations,
    // Short, because the number this page exists to show is one that changes
    // on its own: the gateway catching up is the event being waited for.
    refetchInterval: 3000,
  });
  const data = q.data;

  return (
    <Screen title="Revocations" isLoading={q.isLoading} error={q.error}>
      {data && (
        <>
          <Stats
            items={[
              { k: "Published", v: data.version },
              { k: "Gateway applying", v: data.serving_version },
              {
                k: "In effect",
                v: data.in_effect ? (
                  <span className="badge badge-ok">yes</span>
                ) : (
                  <span className="badge badge-bad">not yet</span>
                ),
              },
              {
                k: "List age",
                v: `${Math.round(data.serving_age_seconds)}s`,
              },
              { k: "Refused", v: data.revocations.length },
            ]}
          />

          {!data.in_effect && (
            <div className="note warn">
              <strong>The gateway has not applied the published list.</strong>{" "}
              Until it does, every credential below still works. This normally
              resolves in a second or two; if it does not, the data plane cannot
              read the list — check that{" "}
              <code>dataplane.revocations_path</code> points at the file the
              control plane writes.
            </div>
          )}

          {data.warning && <div className="note warn">{data.warning}</div>}

          <div className="note">
            <strong>List age is the exposure window.</strong> A revoked
            credential keeps working for as long as the gateway&apos;s list is
            out of date. Under a minute is normal — the control plane
            republishes every 30 seconds. A climbing figure means the gateway
            has stopped receiving the list.
          </div>

          <h2>Refused principals</h2>
          <p className="muted">
            Each of these is refused whatever the snapshot says.
          </p>
          <Table
            columns={["Principal", "Kind", "Reason", "Revoked"]}
            rows={data.revocations.map((r) => [
              <code>{r.principal_id}</code>,
              r.kind === "api_key" ? (
                <span className="badge badge-write">API key</span>
              ) : (
                <span className="badge">session</span>
              ),
              r.reason ?? <span className="muted">—</span>,
              r.revoked_at.replace("T", " ").replace("Z", ""),
            ])}
            empty="Nothing is revoked. An empty list is still published and still signed — it proves the pipeline works before anybody needs it to."
          />

          <p className="muted">
            Entries disappear once a snapshot built after the revocation is
            serving — that snapshot already omits the credential. Pruned through
            snapshot{" "}
            <span className="mono">{data.pruned_through}</span>.
          </p>
        </>
      )}
    </Screen>
  );
}
