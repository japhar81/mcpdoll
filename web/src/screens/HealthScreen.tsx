import { useQuery } from "@tanstack/react-query";
import { getHealth } from "../lib/api.ts";
import { Screen, Stats } from "../components/Screen.tsx";

export function HealthScreen() {
  const q = useQuery({
    queryKey: ["health"],
    queryFn: getHealth,
    refetchInterval: 10_000,
  });

  return (
    <Screen
      title="Control plane health"
      isLoading={q.isLoading}
      error={q.error}
    >
      {q.data && (
        <>
          <Stats
            items={[
              { k: "Status", v: q.data.status },
              { k: "Version", v: q.data.version, small: true },
            ]}
          />
          <h2>What this control plane is reading</h2>
          <p className="muted">
            The files this control plane is reading. Check these first when output
            looks wrong.
          </p>
          <Stats
            items={[
              { k: "Registry", v: q.data.registry_path ?? "—", small: true },
              { k: "Snapshot", v: q.data.snapshot_path ?? "—", small: true },
            ]}
          />
          <div className="note">
            <strong>
This endpoint needs no credential, so a load balancer can reach it.
</strong> A load balancer
            has no credential, and this response says nothing that is not
            already implied by the port accepting connections.
          </div>
        </>
      )}
    </Screen>
  );
}
