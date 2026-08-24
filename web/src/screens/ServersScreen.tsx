import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { listServers } from "../lib/api.ts";
import { EffectBadge, Screen, Table } from "../components/Screen.tsx";

export function ServersScreen() {
  const q = useQuery({ queryKey: ["servers"], queryFn: listServers });

  return (
    <Screen title="Backends" isLoading={q.isLoading} error={q.error}>
      <p className="muted">
        Serving mode is the column to read on a bad day: <code>strict</code>{" "}
        refuses to serve a definition that has drifted from what was admitted,{" "}
        <code>advisory</code> serves it and records the drift.
      </p>
      <p className="muted">
        One backend serves many tenants. The tenant count is how many addresses
        it is bound to; open a backend to see them.
      </p>
      <Table
        columns={[
          "Backend",
          "Namespace",
          "Mode",
          "Default effect",
          "Classification",
          "Tenants",
        ]}
        rows={(q.data?.servers ?? []).map((s) => [
          <Link className="link" to={`/registry/servers/${s.id}`}>
            {s.name}
          </Link>,
          s.namespace,
          s.serving_mode === "advisory" ? (
            <span className="badge badge-write">advisory</span>
          ) : (
            <span className="badge badge-ok">strict</span>
          ),
          <EffectBadge effect={s.default_effect_class} />,
          s.data_classification ?? "—",
          <span className="mono">{s.bindings.length}</span>,
        ])}
        empty="No backends registered."
      />
    </Screen>
  );
}
