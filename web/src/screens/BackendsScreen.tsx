import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { listBackends } from "../lib/api.ts";
import { Screen, Stats, Table } from "../components/Screen.tsx";
import type { BackendHealth } from "../lib/types.ts";

const STATE_BADGE: Record<string, string> = {
  healthy: "badge badge-ok",
  degraded: "badge badge-write",
  unavailable: "badge badge-bad",
  drifted: "badge badge-destructive",
  unknown: "badge",
};

const DRIFT_BADGE: Record<string, string> = {
  semantic: "badge badge-destructive",
  removed: "badge badge-destructive",
  cosmetic: "badge badge-write",
  added: "badge",
};

function blockedCount(b: BackendHealth): number {
  if (b.serving_mode !== "strict") return 0;
  return (b.drift ?? []).filter(
    (d) => d.kind === "semantic" || d.kind === "removed",
  ).length;
}

export function BackendsScreen() {
  const [showDrift, setShowDrift] = useState(false);
  const q = useQuery({
    queryKey: ["gateway", "backends"],
    queryFn: listBackends,
    refetchInterval: 15_000,
    retry: false,
  });
  const report = q.data;

  return (
    <Screen
      title="Backend health"
      actions={
        <label className="inline">
          <input
            type="checkbox"
            checked={showDrift}
            onChange={(e) => setShowDrift(e.target.checked)}
          />
          show every drifted tool
        </label>
      }
      isLoading={q.isLoading}
      error={q.error}
    >
      {report && (
        <>
          <Stats
            items={[
              { k: "Backends", v: report.summary.total },
              { k: "Healthy", v: report.summary.healthy },
              { k: "Degraded", v: report.summary.degraded },
              { k: "Unavailable", v: report.summary.unavailable },
              { k: "Drifted", v: report.summary.drifted },
              { k: "Blocked tools", v: report.summary.blocked_tools },
            ]}
          />

          {report.summary.blocked_tools > 0 && (
            <div className="note warn">
              <strong>
                {report.summary.blocked_tools} tool
                {report.summary.blocked_tools === 1 ? " is" : "s are"} being
                refused.
              </strong>{" "}
              A strict backend has changed its definition since it was admitted,
              so arguments built against the admitted schema may no longer mean
              what they did. Publish a snapshot built from the backends' current
              catalogs, or roll the backends back.
            </div>
          )}

          {report.summary.unknown > 0 && (
            <div className="note">
              {report.summary.unknown} backend
              {report.summary.unknown === 1 ? " has" : "s have"} not been probed
              yet. Unknown is not healthy — a gateway that just started knows
              nothing about what it is fronting.
            </div>
          )}

          {showDrift ? (
            <>
              <h2>Every difference from what was admitted</h2>
              <p className="muted">
                <code>semantic</code> and <code>removed</code> block calls on a
                strict backend. <code>cosmetic</code> never does — the admitted
                description is still accurate for a tool whose schema is
                unchanged, and swapping it would churn every client's prompt
                cache over a reworded sentence.
              </p>
              <Table
                columns={["Backend", "Tool", "Kind", "What changed"]}
                rows={(report.backends ?? []).flatMap((b) =>
                  (b.drift ?? []).map((d) => [
                    b.server_name,
                    d.qualified_name ? (
                      <code>{d.qualified_name}</code>
                    ) : (
                      <>
                        <code>{d.name}</code>{" "}
                        <span className="muted">(unadmitted)</span>
                      </>
                    ),
                    <span className={DRIFT_BADGE[d.kind] ?? "badge"}>
                      {d.kind}
                    </span>,
                    <span className="muted">{d.detail}</span>,
                  ]),
                )}
                empty="Nothing has drifted."
              />
            </>
          ) : (
            <Table
              columns={[
                "Backend",
                "State",
                "Mode",
                "Drift",
                "Blocked",
                "Latency",
                "Protocol",
                "Tools",
              ]}
              rows={(report.backends ?? []).map((b) => [
                <>
                  <strong>{b.server_name}</strong>
                  <br />
                  <code className="muted">{b.endpoint}</code>
                  {b.error && (
                    <>
                      <br />
                      <span className="error small">{b.error}</span>
                    </>
                  )}
                </>,
                <span className={STATE_BADGE[b.state] ?? "badge"}>
                  {b.state}
                </span>,
                b.serving_mode === "advisory" ? (
                  <span className="badge badge-write">advisory</span>
                ) : (
                  <span className="badge badge-ok">strict</span>
                ),
                <span className="mono">{(b.drift ?? []).length}</span>,
                blockedCount(b) > 0 ? (
                  <span className="badge badge-bad">{blockedCount(b)}</span>
                ) : (
                  <span className="mono">0</span>
                ),
                <span className="mono">
                  {b.latency_ewma_ms > 0 ? `${b.latency_ewma_ms}ms` : "—"}
                </span>,
                b.negotiated_version ? (
                  <code>{b.negotiated_version}</code>
                ) : (
                  <span className="muted">—</span>
                ),
                <span className="mono">
                  {b.tools_observed}/{b.tools_admitted}
                </span>,
              ])}
              empty="The prober has not reported on any backend yet."
            />
          )}

          <div className="note">
            <strong>Latency is smoothed.</strong> A single slow probe is noise;
            a rising average is a backend going bad before it starts failing.
            Tools reads <em>observed / admitted</em> — a gap means the backend
            publishes a different number than the snapshot serves.
          </div>
        </>
      )}
    </Screen>
  );
}
