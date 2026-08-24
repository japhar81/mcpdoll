import { EffectBadge, Stats, Table } from "./Screen.tsx";
import type { Snapshot } from "../lib/types.ts";

/** Renders a snapshot. Shared by the current, inspect, and build screens so
 *  three views of the same object cannot disagree about how to show it. */
export function SnapshotView({ snapshot }: { snapshot: Snapshot }) {
  return (
    <>
      {!snapshot.servable && (
        <div className="note warn">
          <strong>This snapshot would not activate.</strong>{" "}
          {snapshot.unservable_reason}
          <br />
          Every data-plane instance would refuse it, so publishing it changes
          nothing — the previous snapshot keeps serving.
        </div>
      )}

      <Stats
        items={[
          { k: "Version", v: snapshot.version },
          { k: "Org", v: snapshot.org, small: true },
          {
            k: "Built",
            v: snapshot.age ? `${snapshot.age} ago` : "—",
            small: true,
          },
          { k: "Signed by", v: snapshot.key_id || "—", small: true },
          { k: "Algorithm", v: snapshot.algorithm || "—", small: true },
          { k: "Tenants", v: snapshot.tenants.length },
        ]}
      />

      {snapshot.source && (
        <p className="muted">
          Source: <code>{snapshot.source}</code> · snapshot id{" "}
          <code>{snapshot.snapshot_id}</code> · registry digest{" "}
          <code>{shortDigest(snapshot.registry_digest)}</code>
        </p>
      )}

      <h2>Tenants</h2>
      <p className="muted">
        Tools admitted per tenant. What any one principal sees is a subset of
        their tenant&apos;s column, decided by their grants — so these counts
        are the ceiling, not the catalog anybody receives.
      </p>
      <Table
        columns={["Tenant", "Name", "Tools admitted", "Tokens"]}
        rows={snapshot.tenants.map((t) => [
          <code>{t.slug}</code>,
          t.name,
          <span className="mono">{t.tools}</span>,
          <span className="mono">{t.token_estimate}</span>,
        ])}
        empty="This snapshot admits nothing for any tenant."
      />

      {snapshot.tools && snapshot.tools.length > 0 && (
        <>
          <h2>Tools ({snapshot.tools.length})</h2>
          <Table
            columns={["Tool", "Backend", "Effect", "Tokens", "Digest"]}
            rows={snapshot.tools.map((t) => [
              <code>{t.qualified_name}</code>,
              t.backend,
              <EffectBadge effect={t.effect_class} />,
              <span className="mono">{t.token_estimate}</span>,
              <code>{shortDigest(t.digest)}</code>,
            ])}
          />
        </>
      )}
    </>
  );
}

function shortDigest(d: string): string {
  if (!d) return "—";
  const value = d.startsWith("sha256:") ? d.slice(7) : d;
  return value.length > 12 ? `${value.slice(0, 12)}…` : value;
}
