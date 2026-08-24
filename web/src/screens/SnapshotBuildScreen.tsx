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
      title="Build a snapshot"
      actions={
        <button
          className="primary"
          disabled={m.isPending}
          onClick={() => m.mutate()}
        >
          {m.isPending
            ? "Discovering…"
            : dryRun
              ? "Dry run"
              : "Build and publish"}
        </button>
      }
    >
      <p className="muted">
        Discovers every backend the registry names, canonicalizes what they
        publish, resolves bundles and audiences, and signs the result. Every
        problem is a build failure: a snapshot some instances would refuse is
        worse than no snapshot.
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
            Tools from a backend that is merely <em>temporarily</em> down would
            vanish from every catalog, invalidating clients' prompt caches. Use
            this to publish around a decommissioned backend, not around an
            outage.
          </div>
        )}
        {!dryRun && (
          <div className="note warn">
            <strong>This publishes.</strong> The data plane picks the new
            snapshot up within a few seconds, with no restart.
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
                : `Published version ${m.data.version}.`}
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
            The negotiated version is how a legacy backend becomes visible
            without anyone having declared it legacy.
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
            A data plane needs this in its <code>trusted_signing_keys</code> to
            accept what was just built.
          </p>
          <pre className="out">{m.data.public_key}</pre>
        </>
      )}
    </Screen>
  );
}
