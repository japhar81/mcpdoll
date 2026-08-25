import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getCurrentSnapshot, getGatewayStatus } from "../lib/api.ts";
import { SnapshotView } from "../components/SnapshotView.tsx";
import { Screen } from "../components/Screen.tsx";

export function SnapshotScreen() {
  const [tools, setTools] = useState(false);
  const q = useQuery({
    queryKey: ["snapshot", "current", tools],
    queryFn: () => getCurrentSnapshot(tools),
  });
  // Freshness, which is a different question from what is serving and the one
  // this screen used to have no answer to. Polled, because the whole claim of
  // this screen is that the thing behind it moves on its own.
  const status = useQuery({
    queryKey: ["gateway", "status"],
    queryFn: getGatewayStatus,
    refetchInterval: 10_000,
  });

  return (
    <Screen
      title="What is serving"
      actions={
        <label className="inline">
          <input
            type="checkbox"
            checked={tools}
            onChange={(e) => setTools(e.target.checked)}
          />
          list every tool
        </label>
      }
      isLoading={q.isLoading}
      error={q.error}
    >
      <Freshness
        ageSeconds={status.data?.catalog_age_seconds}
        error={status.data?.catalog_error}
      />
      {q.data && <SnapshotView snapshot={q.data} />}
    </Screen>
  );
}

// How long ago the catalog was last rebuilt — deliberately not how long ago it
// changed. A version only moves on change, so on its own it cannot tell a
// deployment where nothing has happened from one whose rebuild loop has died.
function Freshness(props: { ageSeconds?: number; error?: string }) {
  if (props.error) {
    return (
      <div className="note bad">
        <strong>The last rebuild failed.</strong> The catalog below is the last
        one that worked and is still being served — that is deliberate, an
        unreachable backend must not empty the gateway. It is going stale until
        this clears: {props.error}
      </div>
    );
  }
  if (props.ageSeconds === undefined) {
    return null;
  }
  return (
    <div className="note">
      Rebuilt from the backends {describeAge(props.ageSeconds)}. Nothing to
      publish — a change to the registry, a tenant, or a backend&apos;s own
      tools reaches the gateway on its own.
    </div>
  );
}

function describeAge(seconds: number) {
  if (seconds < 90) {
    return `${Math.round(seconds)}s ago`;
  }
  const minutes = Math.round(seconds / 60);
  return minutes < 90 ? `${minutes}m ago` : `${Math.round(minutes / 60)}h ago`;
}
