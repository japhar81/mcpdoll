import { useQuery } from "@tanstack/react-query";
import { getGatewayStatus } from "../lib/api.ts";
import { Screen, Stats } from "../components/Screen.tsx";

export function GatewayScreen() {
  const q = useQuery({
    queryKey: ["gateway", "status"],
    queryFn: getGatewayStatus,
    refetchInterval: 10_000,
    retry: false,
  });

  return (
    <Screen title="Data plane" isLoading={q.isLoading} error={q.error}>
      {q.data && (
        <>
          <Stats
            items={[
              {
                k: "Ready",
                v: q.data.ready ? (
                  <span className="badge badge-ok">yes</span>
                ) : (
                  <span className="badge badge-bad">no</span>
                ),
              },
              { k: "Snapshot", v: q.data.snapshot_version },
              { k: "Audiences", v: q.data.audiences },
              { k: "URL", v: <code>{q.data.gateway_url}</code>, small: true },
            ]}
          />
          <div className="note">
            <strong>
              The data plane does not depend on this control plane at request
              time.
            </strong>{" "}
            It serves from the snapshot it already holds, so a control-plane
            outage stops publishing and stops this console — it does not stop a
            single agent's tool call.
          </div>
        </>
      )}
    </Screen>
  );
}
