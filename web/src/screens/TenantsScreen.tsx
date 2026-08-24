import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { listTenants } from "../lib/api.ts";
import { Screen, Stats, Table } from "../components/Screen.tsx";

/**
 * Tenants, as the registry declares them and as the gateway is serving them.
 *
 * Two sources deliberately: the registry is what the *next* snapshot will
 * publish, the gateway is what is being served *now*. When they disagree,
 * somebody edited the registry and did not publish, and the disagreement is the
 * useful thing on this screen.
 */
export function TenantsScreen() {
  const q = useQuery({
    queryKey: ["gateway", "tenants"],
    queryFn: listTenants,
  });
  const data = q.data;

  return (
    <Screen title="Tenants" isLoading={q.isLoading} error={q.error}>
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
              { k: "Serving tenants", v: data.tenants },
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

          {data.ready && data.tenants !== data.registered.length && (
            <div className="note warn">
              <strong>
                The gateway is serving {data.tenants} tenant
                {data.tenants === 1 ? "" : "s"}, but {data.registered.length}{" "}
                {data.registered.length === 1 ? "is" : "are"} registered.
              </strong>{" "}
              The snapshot it holds predates the current registry — build and
              publish to reconcile.
            </div>
          )}

          <h2>Registered</h2>
          <p className="muted">
            Tool counts are what a tenant <em>admits</em>. No principal
            necessarily receives all of them: a catalog is the intersection of
            this column with that principal&apos;s grants, which is why the way
            to see one is to present a key rather than to read a table.
          </p>
          <Table
            columns={["Slug", "Name", "Status", "Tools admitted", ""]}
            rows={data.registered.map((t) => [
              <code>{t.slug}</code>,
              t.name,
              t.status === "serving" ? (
                <span className="badge badge-ok">serving</span>
              ) : (
                <span className="badge">{t.status}</span>
              ),
              <span className="mono">{t.tools}</span>,
              <Link className="link" to="/gateway/catalog">
                inspect a principal →
              </Link>,
            ])}
            empty="The registry declares no tenants."
          />
        </>
      )}
    </Screen>
  );
}
