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
            Two paths, shown because unexpected output is nearly always the
            wrong file rather than the wrong logic.
          </p>
          <Stats
            items={[
              { k: "Registry", v: q.data.registry_path ?? "—", small: true },
              { k: "Snapshot", v: q.data.snapshot_path ?? "—", small: true },
            ]}
          />
          <div className="note">
            <strong>Health is outside the auth wall.</strong> A load balancer
            has no credential, and this response says nothing that is not
            already implied by the port accepting connections.
          </div>
        </>
      )}
    </Screen>
  );
}
