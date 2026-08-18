import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { listAudiences } from "../lib/api.ts";
import { Screen, Stats, Table } from "../components/Screen.tsx";

export function AudiencesScreen() {
  const q = useQuery({
    queryKey: ["gateway", "audiences"],
    queryFn: listAudiences,
  });
  const data = q.data;

  return (
    <Screen title="Audiences" isLoading={q.isLoading} error={q.error}>
      {data && (
        <>
          <Stats
            items={[
              {
                k: "Gateway",
                v: data.ready ? (
                  <span className="badge badge-ok">ready</span>
                ) : (
                  <span className="badge badge-bad">not ready</span>
                ),
              },
              { k: "Serving snapshot", v: data.snapshot_version },
              { k: "Serving audiences", v: data.audiences },
              { k: "Registered", v: data.registered.length },
            ]}
          />

          {!data.ready && (
            <div className="note warn">
              The data plane could not be reached, so the live counts above are
              zero. The table below is still accurate — it is what the registry
              declares.
            </div>
          )}

          {data.ready && data.audiences !== data.registered.length && (
            <div className="note warn">
              <strong>
                The gateway is serving {data.audiences} audience
                {data.audiences === 1 ? "" : "s"}, but {data.registered.length}{" "}
                {data.registered.length === 1 ? "is" : "are"} registered.
              </strong>{" "}
              The snapshot it holds predates the current registry — build and
              publish to reconcile.
            </div>
          )}

          <h2>Registered</h2>
          <p className="muted">
            From the registry, not from the gateway: the data plane reports a
            count rather than names, because enumerating endpoints to an
            unauthenticated caller would tell an attacker which ones exist.
          </p>
          <Table
            columns={["Slug", "Name", "Bundles", "Allowed groups", ""]}
            rows={data.registered.map((a) => [
              <code>{a.slug}</code>,
              a.name ?? "",
              a.bundles.join(", "),
              a.allowed_idp_groups?.length ? (
                a.allowed_idp_groups.join(", ")
              ) : (
                <span className="muted">any authenticated</span>
              ),
              <>
                <Link
                  className="link"
                  to={`/gateway/audiences/${a.slug}/catalog`}
                >
                  catalog
                </Link>
                {" · "}
                <Link
                  className="link"
                  to={`/gateway/audiences/${a.slug}/playground`}
                >
                  call a tool
                </Link>
              </>,
            ])}
            empty="No audiences registered."
          />
        </>
      )}
    </Screen>
  );
}
