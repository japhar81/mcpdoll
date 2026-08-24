import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { getRegistry } from "../lib/api.ts";
import { EffectBadge, Screen, Stats, Table } from "../components/Screen.tsx";

export function RegistryScreen() {
  const q = useQuery({ queryKey: ["registry"], queryFn: getRegistry });
  const reg = q.data;

  return (
    <Screen title="Registry" isLoading={q.isLoading} error={q.error}>
      {reg && (
        <>
          <Stats
            items={[
              { k: "Org", v: reg.org, small: true },
              { k: "Version", v: reg.version },
              { k: "Namespaces", v: reg.namespaces.length },
              { k: "Backends", v: reg.servers.length },
              { k: "Toolsets", v: reg.toolsets.length },
              { k: "Policies", v: reg.policies?.length ?? 0 },
              { k: "Plugins", v: reg.plugins?.length ?? 0 },
            ]}
          />

          <h2>Namespaces</h2>
          <p className="muted">
            Every tool in a namespace is served under its prefix, so no two
            namespaces may share one.
          </p>
          <Table
            columns={["Prefix", "Name", "Team", "Owner group"]}
            rows={reg.namespaces.map((n) => [
              <code>{n.prefix}</code>,
              n.name,
              n.team ?? "—",
              n.owner_idp_group ?? "—",
            ])}
          />

          <h2>Toolsets</h2>
          <p className="muted">
            The grantable unit. A toolset&apos;s name appears in every grant
            scope that reaches it, so renaming one changes who can reach it —
            regrant afterwards. Priority breaks ties when two toolsets carry the
            same tool.
          </p>
          <Table
            columns={["Name", "Priority", "Namespaces", "Token budget"]}
            rows={reg.toolsets.map((ts) => [
              <code>{ts.name}</code>,
              <span className="mono">{ts.priority}</span>,
              ts.namespaces.join(", "),
              ts.token_budget ? (
                <span className="mono">{ts.token_budget}</span>
              ) : (
                <span className="muted">unbounded</span>
              ),
            ])}
            empty="The registry declares no toolsets, so no grant can name one."
          />

          <h2>Backends</h2>
          <Table
            columns={[
              "Backend",
              "Namespace",
              "Tenants",
              "Mode",
              "Default effect",
              "Classification",
            ]}
            rows={reg.servers.map((s) => [
              <Link className="link" to={`/registry/servers/${s.id}`}>
                {s.name}
              </Link>,
              s.namespace,
              <span className="mono">{s.bindings.length}</span>,
              s.serving_mode,
              <EffectBadge effect={s.default_effect_class} />,
              s.data_classification ?? "—",
            ])}
          />

          {(reg.policies?.length ?? 0) > 0 && (
            <>
              <h2>Policies</h2>
              <Table
                columns={["Name", "Priority", "Rules"]}
                rows={(reg.policies ?? []).map((p) => [
                  p.name,
                  <span className="mono">{p.priority}</span>,
                  <span className="mono">{p.rule_count}</span>,
                ])}
              />
            </>
          )}
        </>
      )}
    </Screen>
  );
}
