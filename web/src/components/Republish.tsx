import { useMutation } from "@tanstack/react-query";

import { buildSnapshot } from "../lib/api.ts";
import { ErrorBlock } from "./Screen.tsx";

/**
 * The button that makes a grant change take effect.
 *
 * Grants travel in the signed snapshot (ADR 0018), so editing one changes the
 * database and nothing the data plane can see until a rebuild lands. That is
 * what keeps a control-plane outage invisible to a tool call, and it means an
 * admin who changes something and walks away has changed nothing yet — so the
 * publish control lives next to the change rather than three screens away.
 *
 * The banner says what to do, not why. The reasoning is here.
 */
export function Republish({ what }: { what: string }) {
  const build = useMutation({ mutationFn: () => buildSnapshot({}) });

  return (
    <div className="note warn">
      <strong>{what} take effect at the next snapshot, not now.</strong>{" "}
      Publish to apply them.
      {build.error != null && <ErrorBlock error={build.error} />}
      {build.data && (
        <p className="muted">
          Published snapshot {build.data.version} with {build.data.tools} tool
          {build.data.tools === 1 ? "" : "s"}. The gateway picks it up within a
          few seconds.
        </p>
      )}
      <div className="row">
        <span className="spacer" />
        <button
          className="primary"
          disabled={build.isPending}
          onClick={() => build.mutate()}
        >
          {build.isPending ? "Publishing…" : "Publish now"}
        </button>
      </div>
    </div>
  );
}
