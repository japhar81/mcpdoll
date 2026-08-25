import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";

import {
  getCurrentSnapshot,
  getGatewayStatus,
  getHealth,
  listBackends,
} from "../lib/api.ts";
import { Screen } from "../components/Screen.tsx";

/**
 * What the pieces are, and which one you are looking at.
 *
 * MCPDoll is two planes and they are easy to confuse — both serve HTTP, both
 * are "MCPDoll", and the console lists four URLs. The distinction between them
 * is the whole architecture (ADR 0002), so it is said here once, plainly, with
 * each piece's live state beside it rather than in a README nobody opens.
 */
export function OverviewScreen() {
  const control = useQuery({
    queryKey: ["health"],
    queryFn: getHealth,
    retry: false,
  });
  const gateway = useQuery({
    queryKey: ["gateway", "status"],
    queryFn: getGatewayStatus,
    retry: false,
    refetchInterval: 15_000,
  });
  const backends = useQuery({
    queryKey: ["gateway", "backends"],
    queryFn: listBackends,
    retry: false,
    refetchInterval: 15_000,
  });
  const snapshot = useQuery({
    queryKey: ["snapshot", "current", false],
    queryFn: () => getCurrentSnapshot(false),
    retry: false,
  });

  const summary = backends.data?.summary;
  const toolCount = snapshot.data?.tenants.reduce((n, t) => n + t.tools, 0);

  return (
    <Screen title="Overview">
      <p className="muted">
        MCPDoll fronts many MCP backends behind one MCP endpoint. Who you are
        decides what you see: a principal's grants are their catalog.
      </p>

      <div className="topology">
        <Plane
          role="Agents connect here"
          name="Data plane"
          url="http://localhost:8080"
          urlNote="/mcp — one endpoint; the credential decides the catalog"
          state={
            gateway.data?.ready
              ? { label: "serving", tone: "ok" }
              : {
                  label: gateway.isLoading ? "checking" : "not ready",
                  tone: "bad",
                }
          }
          points={[
            "Serves tools from one signed snapshot held in memory.",
            "Has no dependency on the control plane at request time — a control-plane outage stops publishing, not tool calls.",
            "This is the only address an agent ever needs.",
          ]}
          facts={[
            [
              "Snapshot",
              gateway.data ? String(gateway.data.snapshot_version) : "—",
            ],
            ["Tenants", gateway.data ? String(gateway.data.tenants) : "—"],
            ["Tools served", toolCount === undefined ? "—" : String(toolCount)],
          ]}
          link={{ to: "/gateway", label: "data plane status →" }}
        />

        <Plane
          role="Operators and this console connect here"
          name="Control plane"
          url="http://localhost:3001"
          urlNote="the API behind this console and the CLI"
          state={
            control.data
              ? { label: "ok", tone: "ok" }
              : {
                  label: control.isLoading ? "checking" : "unreachable",
                  tone: "bad",
                }
          }
          points={[
            "Reads the registry, builds and signs snapshots, and holds the signing key.",
            "Never in an agent's request path. Everything on this console comes from here.",
            "The CLI talks to the same API — anything it can do, your own tooling can do.",
          ]}
          facts={[
            ["Registry", control.data?.registry_path ?? "—"],
            ["Snapshot file", control.data?.snapshot_path ?? "—"],
          ]}
          link={{ to: "/system/health", label: "control plane health →" }}
        />

        <Plane
          role="Operators only — never agents"
          name="Data plane admin"
          url="http://localhost:8081"
          urlNote="/admin/backends — what the prober knows"
          state={
            summary
              ? summary.blocked_tools > 0
                ? { label: `${summary.blocked_tools} blocked`, tone: "bad" }
                : { label: "clean", tone: "ok" }
              : { label: "—", tone: "muted" }
          }
          points={[
            "A separate port from the tool endpoint, deliberately.",
            "It reports every backend and its address — an inventory of what is behind the gateway, which an agent that can call a tool has no business reading.",
            "In a real deployment this would not leave the internal network.",
          ]}
          facts={[
            ["Backends", summary ? String(summary.total) : "—"],
            ["Healthy", summary ? String(summary.healthy) : "—"],
            ["Drifted", summary ? String(summary.drifted) : "—"],
          ]}
          link={{ to: "/gateway/backends", label: "backend health →" }}
        />

        <Plane
          role="Where the telemetry goes"
          name="Grafana"
          url="http://localhost:3300"
          urlNote="folder: MCPDoll"
          state={{ label: "external", tone: "muted" }}
          points={[
            "Traces, metrics and logs from both planes, correlated by trace id.",
            "The MCPDoll folder holds the gateway dashboard: serving, backends, and the plugin pipeline.",
            "Anonymous access is on locally, so it needs no login.",
          ]}
          facts={[]}
          external="http://localhost:3300"
        />
      </div>

      <h2>How a tool call flows</h2>
      <ol className="flow">
        <li>
          An agent connects to <code>/mcp</code> with its credential. The
          gateway resolves that to a principal, composes the catalog from that
          principal&apos;s grants, and serves <strong>admitted</strong>{" "}
          definitions from the snapshot — never whatever a backend currently
          reports.
        </li>
        <li>
          It calls a tool. The seven-hook pipeline runs: plugins may deny,
          mutate, annotate, or defer to a human before the call leaves.
        </li>
        <li>
          The data plane dispatches to the backend using a per-backend
          credential. The agent&apos;s inbound token is never forwarded.
        </li>
        <li>
          The result returns through the pipeline — where redaction runs — and
          only then reaches the agent.
        </li>
      </ol>

      <div className="note">
        <strong>The catalog keeps itself current.</strong> Backends are
        re-read on a timer, and a change — a new tool, an edited registry, a
        new tenant — reaches the gateway on its own with no restart and nothing
        to publish. What is serving right now is on{" "}
        <Link className="link" to="/snapshots">
          Catalog
        </Link>
        . Nothing here interrupts a request in flight.
      </div>
    </Screen>
  );
}

function Plane(props: {
  role: string;
  name: string;
  url: string;
  urlNote: string;
  state: { label: string; tone: "ok" | "bad" | "muted" };
  points: string[];
  facts: Array<[string, string]>;
  link?: { to: string; label: string };
  external?: string;
}) {
  const toneClass =
    props.state.tone === "ok"
      ? "badge badge-ok"
      : props.state.tone === "bad"
        ? "badge badge-bad"
        : "badge";

  return (
    <section className="plane">
      <div className="plane-head">
        <div>
          <div className="plane-role">{props.role}</div>
          <h3>{props.name}</h3>
        </div>
        <span className={toneClass}>{props.state.label}</span>
      </div>

      <div className="plane-url">
        {props.external ? (
          <a
            className="link mono"
            href={props.external}
            target="_blank"
            rel="noreferrer"
          >
            {props.url}
          </a>
        ) : (
          <code>{props.url}</code>
        )}
        <div className="muted">{props.urlNote}</div>
      </div>

      <ul className="plane-points">
        {props.points.map((p, i) => (
          <li key={i}>{p}</li>
        ))}
      </ul>

      {props.facts.length > 0 && (
        <dl className="plane-facts">
          {props.facts.map(([k, v]) => (
            <div key={k}>
              <dt>{k}</dt>
              <dd className="mono">{v}</dd>
            </div>
          ))}
        </dl>
      )}

      {props.link && (
        <Link className="link plane-link" to={props.link.to}>
          {props.link.label}
        </Link>
      )}
    </section>
  );
}
