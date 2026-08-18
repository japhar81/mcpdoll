/**
 * Shared screen chrome: title, loading and error states, content slot.
 *
 * Ported from RAGdoll's Screen.tsx. The error branch is the part that matters:
 * an API error carries a list of problems, and rendering them as a list rather
 * than a blob is what makes a registry with six faults one round trip to fix.
 */
import type { ReactNode } from "react";
import { ApiError } from "../lib/api.ts";

export function Screen(props: {
  title: string;
  actions?: ReactNode;
  isLoading?: boolean;
  error?: unknown;
  children: ReactNode;
}) {
  return (
    <section className="screen">
      <header className="toolbar">
        <strong>{props.title}</strong>
        <span className="spacer" />
        {props.actions}
      </header>
      <div className="screen-body">
        {props.isLoading && <p className="muted">Loading…</p>}
        {props.error != null && <ErrorBlock error={props.error} />}
        {!props.isLoading && props.error == null && props.children}
      </div>
    </section>
  );
}

export function ErrorBlock({ error }: { error: unknown }) {
  if (error instanceof ApiError) {
    return (
      <div className="error">
        <div>
          {error.status > 0 ? `HTTP ${error.status} — ` : ""}
          {error.message}
        </div>
        {error.problems.length > 0 && (
          <ul>
            {error.problems.map((p, i) => (
              <li key={i}>{p}</li>
            ))}
          </ul>
        )}
        {error.status === 401 && (
          <div className="note warn">
            <strong>No credential.</strong> Set the API token in the sidebar.
            The control plane refuses to run without one unless it was started
            with <code>--allow-anonymous</code>.
          </div>
        )}
      </div>
    );
  }
  return (
    <p className="error">
      {error instanceof Error ? error.message : String(error)}
    </p>
  );
}

export function Table(props: {
  columns: string[];
  rows: ReactNode[][];
  empty?: string;
}) {
  return (
    <table className="grid">
      <thead>
        <tr>
          {props.columns.map((c) => (
            <th key={c}>{c}</th>
          ))}
        </tr>
      </thead>
      <tbody>
        {props.rows.length === 0 && (
          <tr>
            <td colSpan={props.columns.length} className="muted">
              {props.empty ?? "Nothing to show."}
            </td>
          </tr>
        )}
        {props.rows.map((row, i) => (
          <tr key={i}>
            {row.map((cell, j) => (
              <td key={j}>{cell}</td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function Stats(props: {
  items: Array<{ k: string; v: ReactNode; small?: boolean }>;
}) {
  return (
    <div className="stats">
      {props.items.map((item) => (
        <div className="stat" key={item.k}>
          <div className="k">{item.k}</div>
          <div className={item.small ? "v small" : "v"}>{item.v}</div>
        </div>
      ))}
    </div>
  );
}

/** An effect class rendered so its risk is visible without reading the word. */
export function EffectBadge({ effect }: { effect: string }) {
  const known = ["destructive", "write", "read"].includes(effect);
  return (
    <span className={known ? `badge badge-${effect}` : "badge"}>
      {effect || "—"}
    </span>
  );
}

/**
 * A rollout state.
 *
 * `shadow` is deliberately the muted grey and `enforce` the saturated blue: the
 * question an operator is asking when they scan this column is "what is
 * actually acting on traffic?", and that should be the thing that stands out.
 */
export function RolloutBadge({
  rollout,
  canary,
}: {
  rollout: string;
  canary?: number;
}) {
  if (rollout === "canary") {
    return <span className="badge badge-canary">canary {canary ?? 0}%</span>;
  }
  if (rollout === "enforce")
    return <span className="badge badge-enforce">enforce</span>;
  return <span className="badge">{rollout || "shadow"}</span>;
}
