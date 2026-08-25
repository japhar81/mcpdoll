import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { buildSnapshot } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats, Table } from "../components/Screen.tsx";

export function SnapshotBuildScreen() {
  const [dryRun, setDryRun] = useState(true);
  const [allowUnreachable, setAllowUnreachable] = useState(false);
  const client = useQueryClient();

  const m = useMutation({
    mutationFn: () =>
      buildSnapshot({ dry_run: dryRun, allow_unreachable: allowUnreachable }),
    onSuccess: (report) => {
      // A real build replaces what the gateway serves within seconds, so the
      // cached "current snapshot" is stale the moment this returns.
      if (!report.dry_run) {
        void client.invalidateQueries({ queryKey: ["snapshot"] });
        void client.invalidateQueries({ queryKey: ["gateway"] });
      }
    },
  });

  return (
    <Screen
      title="Rebuild now"
      actions={
        <button
          className="primary"
          disabled={m.isPending}
          onClick={() => m.mutate()}
        >
          {m.isPending ? "Discovering…" : dryRun ? "Dry run" : "Rebuild now"}
        </button>
      }
    >
      <p className="muted">
        This happens on a timer already — the catalog does not wait for anyone.
        Use this when you have just changed something and would rather not wait
        out the interval. It discovers every backend the registry names,
        resolves toolsets and per-tenant bindings, and signs the result; any
        problem fails the rebuild rather than serving a half-resolved catalog.
      </p>

      <div className="card">
        <label className="inline">
          <input
            type="checkbox"
            checked={dryRun}
            onChange={(e) => setDryRun(e.target.checked)}
          />
          Dry run — validate and report without writing the file
        </label>
        <label className="inline">
          <input
            type="checkbox"
            checked={allowUnreachable}
            onChange={(e) => setAllowUnreachable(e.target.checked)}
          />
          Allow unreachable backends, omitting their tools
        </label>
        {allowUnreachable && (
          <div className="note warn">
            Their tools vanish from every catalog that had them. Use this for
            a decommissioned backend, not for one that is briefly down.
          </div>
        )}

      </div>

      {m.error != null && <ErrorBlock error={m.error} />}

      {m.data && (
        <>
          <div className="note">
            <strong>
              {m.data.dry_run
                ? "Built (not written)."
                : m.data.unchanged
                  ? "Nothing changed — the catalog already matched."
                  : `Now serving version ${m.data.version}.`}
            </strong>{" "}
            {m.data.output && (
              <>
                Written to <code>{m.data.output}</code>.
              </>
            )}
          </div>

          <Stats
            items={[
              { k: "Version", v: m.data.version },
              { k: "Tools", v: m.data.tools },
              { k: "Backends", v: m.data.servers },
              { k: "Toolsets", v: m.data.toolsets },
              { k: "Plugins", v: m.data.plugins },
              { k: "Signed by", v: m.data.key_id, small: true },
            ]}
          />

          {(m.data.warnings?.length ?? 0) > 0 && (
            <div className="note warn">
              <strong>Warnings</strong>
              <ul>
                {(m.data.warnings ?? []).map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            </div>
          )}

          <h2>What discovery found</h2>
          <p className="muted">
            Protocol is what each backend negotiated, which may be older than
            what the gateway serves its own clients.
          </p>
          <Table
            columns={[
              "Backend",
              "Negotiated",
              "Published",
              "Admitted",
              "Excluded",
            ]}
            rows={m.data.backends.map((b) => [
              <>
                {b.server_name}
                <br />
                <code className="muted">{b.endpoint}</code>
              </>,
              b.negotiated_version ? (
                <code>{b.negotiated_version}</code>
              ) : (
                <span className="muted">—</span>
              ),
              <span className="mono">{b.tool_count}</span>,
              <span className="mono">{b.admitted.length}</span>,
              b.excluded?.length ? (
                <span className="mono">{b.excluded.length}</span>
              ) : (
                <span className="muted">0</span>
              ),
            ])}
          />

          <h2>Trust entry</h2>
          <p className="muted">
            Each data plane needs this in its{" "}
            <code>trusted_signing_keys</code> to accept what was just built.
          </p>
          <pre className="out">{m.data.public_key}</pre>
        </>
      )}
    </Screen>
  );
}
