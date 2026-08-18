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
              { k: "Bundles", v: reg.bundles.length },
              { k: "Audiences", v: reg.audiences.length },
              { k: "Policies", v: reg.policies?.length ?? 0 },
              { k: "Plugins", v: reg.plugins?.length ?? 0 },
            ]}
          />

          <h2>Audiences</h2>
          <p className="muted">
            One MCP endpoint each. The groups column is who may connect — blank
            means any authenticated principal.
          </p>
          <Table
            columns={["Slug", "Name", "Bundles", "Allowed groups", ""]}
            rows={reg.audiences.map((a) => [
              <code>{a.slug}</code>,
              a.name ?? "",
              a.bundles.join(", "),
              a.allowed_idp_groups?.length ? (
                a.allowed_idp_groups.join(", ")
              ) : (
                <span className="muted">any authenticated</span>
              ),
              <Link
                className="link"
                to={`/gateway/audiences/${a.slug}/catalog`}
              >
                inspect →
              </Link>,
            ])}
          />

          <h2>Namespaces</h2>
          <p className="muted">
            The prefix is what every tool in the namespace is served under, and
            is why two namespaces cannot share one.
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

          <h2>Bundles</h2>
          <Table
            columns={["Name", "Priority", "Namespaces", "Token budget"]}
            rows={reg.bundles.map((b) => [
              b.name,
              <span className="mono">{b.priority}</span>,
              b.namespaces.join(", "),
              b.token_budget ? (
                <span className="mono">{b.token_budget}</span>
              ) : (
                <span className="muted">unbounded</span>
              ),
            ])}
          />

          <h2>Backends</h2>
          <Table
            columns={[
              "Backend",
              "Namespace",
              "Mode",
              "Default effect",
              "Classification",
            ]}
            rows={reg.servers.map((s) => [
              <Link className="link" to={`/registry/servers/${s.id}`}>
                {s.name}
              </Link>,
              s.namespace,
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
