import { useQuery } from "@tanstack/react-query";
import { listPlugins } from "../lib/api.ts";
import { RolloutBadge, Screen, Table } from "../components/Screen.tsx";

export function PluginsScreen() {
  const q = useQuery({ queryKey: ["plugins"], queryFn: listPlugins });
  const plugins = q.data?.plugins ?? [];
  const enforcing = plugins.filter((p) => p.rollout === "enforce");
  const identityDependent = plugins.filter((p) => p.identity_dependent);

  return (
    <Screen title="Plugins" isLoading={q.isLoading} error={q.error}>
      <p className="muted">
        Rollout is the column to read first. shadow runs a plugin and records what
        it would have done without doing it; enforce acts. Read a plugin's shadow
        divergences before promoting it.
      <code>shadow</code> means a plugin
        runs and is recorded but changes nothing; <code>enforce</code> means it
        acts. Promote only after reading a plugin's shadow divergences.
      </p>
      <Table
        columns={[
          "Plugin",
          "Runtime",
          "Hooks",
          "Priority",
          "Rollout",
          "Writes",
        ]}
        rows={plugins.map((p) => [
          <>
            <strong>{p.name}</strong>
            {p.version && <span className="muted"> {p.version}</span>}
          </>,
          p.runtime,
          <code>{p.hooks.join(", ")}</code>,
          <span className="mono">{p.priority}</span>,
          <RolloutBadge rollout={p.rollout} canary={p.canary_percent} />,
          p.writes?.length ? (
            <code>{p.writes.join(", ")}</code>
          ) : (
            <span className="muted">read-only</span>
          ),
        ])}
        empty="No plugins registered."
      />

      {plugins.length > 0 && (
        <p className="muted" style={{ marginTop: 10 }}>
          {plugins.length} plugin{plugins.length === 1 ? "" : "s"},{" "}
          {enforcing.length} enforcing.
        </p>
      )}

      {identityDependent.length > 0 && (
        <div className="note">
          <strong>{identityDependent.map((p) => p.name).join(", ")}</strong>{" "}
          {identityDependent.length === 1 ? "is" : "are"} identity-dependent. At{" "}
          <code>on_catalog</code> that forces <code>cacheScope: private</code>,
          because a catalog shaped by who is asking must never be cached
          publicly.
        </div>
      )}

      <h2>Artifact digests</h2>
      <p className="muted">
        Checked before load. A mismatch refuses the plugin, so update the digest
        whenever you rebuild one.
      </p>
      <Table
        columns={["Plugin", "Digest"]}
        rows={plugins.map((p) => [
          p.name,
          p.artifact_digest ? (
            <code>{p.artifact_digest}</code>
          ) : (
            <span className="muted">none</span>
          ),
        ])}
      />
    </Screen>
  );
}
