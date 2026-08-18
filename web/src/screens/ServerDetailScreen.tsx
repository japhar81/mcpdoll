import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { getServer } from "../lib/api.ts";
import { EffectBadge, Screen, Stats, Table } from "../components/Screen.tsx";

export function ServerDetailScreen() {
  const { serverId = "" } = useParams();
  const q = useQuery({
    queryKey: ["server", serverId],
    queryFn: () => getServer(serverId),
  });
  const s = q.data;

  const overrides = Object.entries(s?.tool_overrides ?? {});
  const excluded = s?.excluded_tools ?? [];

  return (
    <Screen
      title={s ? s.name : serverId}
      actions={
        <Link className="link" to="/registry/servers">
          ← all backends
        </Link>
      }
      isLoading={q.isLoading}
      error={q.error}
    >
      {s && (
        <>
          <Stats
            items={[
              { k: "Namespace", v: s.namespace, small: true },
              { k: "Serving mode", v: s.serving_mode, small: true },
              {
                k: "Default effect",
                v: <EffectBadge effect={s.default_effect_class} />,
                small: true,
              },
              {
                k: "Classification",
                v: s.data_classification ?? "—",
                small: true,
              },
              { k: "Criticality", v: s.criticality ?? "—", small: true },
              { k: "Endpoint", v: <code>{s.endpoint}</code>, small: true },
            ]}
          />

          {s.compliance_scope?.length ? (
            <div className="note">
              <strong>Compliance scope:</strong> {s.compliance_scope.join(", ")}
            </div>
          ) : null}

          <h2>Tool classification</h2>
          <p className="muted">
            Only the tools the registry names explicitly. Everything else this
            backend publishes inherits{" "}
            <EffectBadge effect={s.default_effect_class} />, which is why the
            default is the field to get right.
          </p>
          <Table
            columns={["Tool", "Effect class"]}
            rows={overrides.map(([name, effect]) => [
              <code>{name}</code>,
              <EffectBadge effect={effect} />,
            ])}
            empty="No per-tool overrides — every tool uses the default."
          />

          {excluded.length > 0 && (
            <>
              <h2>Withheld tools</h2>
              <p className="muted">
                Never admitted, for any audience. An exclusion is not an
                override with a blank class: the tool does not reach the
                snapshot at all.
              </p>
              <Table
                columns={["Tool"]}
                rows={excluded.map((name) => [<code>{name}</code>])}
              />
            </>
          )}

          {s.canary_tool && (
            <div className="note">
              <strong>Canary tool:</strong> <code>{s.canary_tool}</code> —
              probed to decide whether this backend is healthy, rather than
              trusting a transport-level ping.
            </div>
          )}
        </>
      )}
    </Screen>
  );
}
