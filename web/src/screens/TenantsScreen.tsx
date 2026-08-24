import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router-dom";

import { createTenant, deleteTenant, listTenants } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats, Table } from "../components/Screen.tsx";

/**
 * Tenants, joined across the three places a tenant exists.
 *
 * One screen for three routes: the list, the create form, and the delete
 * confirmation. They are one page because they are one decision — an operator
 * looking at the list is the person who adds or removes a row — and because a
 * confirmation that does not show what it is about is not a confirmation.
 */
export function TenantsScreen() {
  const { tenantId } = useParams();
  const creating = location.pathname.endsWith("/new");

  const client = useQueryClient();
  const navigate = useNavigate();
  const q = useQuery({ queryKey: ["tenants"], queryFn: listTenants });

  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");

  const create = useMutation({
    mutationFn: () => createTenant(slug.trim(), name.trim() || slug.trim()),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["tenants"] });
      setSlug("");
      setName("");
      navigate("/tenants");
    },
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteTenant(id),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["tenants"] });
      navigate("/tenants");
    },
  });

  const data = q.data;
  const doomed = data?.registered.find((t) => t.id === tenantId);

  // Named, not counted. "Some tenants have no bindings" makes the reader scan
  // the table to work out which — the banner should say what the row already
  // knows.
  const unregistered = (data?.registered ?? [])
    .filter((t) => t.status === "unregistered")
    .map((t) => t.slug);
  const unbound = (data?.registered ?? [])
    .filter((t) => t.id && t.backends === 0)
    .map((t) => t.slug);

  return (
    <Screen
      title="Tenants"
      isLoading={q.isLoading}
      error={q.error}
      actions={
        !creating && (
          <Link className="link" to="/tenants/new">
            new tenant
          </Link>
        )
      }
    >
      {creating && (
        <div className="card">
          <div className="card-head">
            <strong>New tenant</strong>
          </div>
          <p className="muted">
            The slug appears in every grant scope for this tenant and cannot be
            changed afterwards.
          </p>
          <label className="field">
            Slug
            <input
              spellCheck={false}
              placeholder="acme"
              value={slug}
              onChange={(e) => setSlug(e.target.value)}
            />
          </label>
          <label className="field">
            Display name
            <input
              placeholder="Acme Corporation"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          {create.error != null && <ErrorBlock error={create.error} />}
          <div className="row">
            <Link className="link" to="/tenants">
              cancel
            </Link>
            <span className="spacer" />
            <button
              className="primary"
              disabled={!slug.trim() || create.isPending}
              onClick={() => create.mutate()}
            >
              {create.isPending ? "Creating…" : "Create"}
            </button>
          </div>
        </div>
      )}

      {tenantId && (
        <div className="card">
          <div className="card-head">
            <strong>Delete {doomed?.slug ?? tenantId}?</strong>
            <span className="badge badge-bad">irreversible</span>
          </div>
          {doomed ? (
            <p className="muted">
              This deletes the tenant along with its {doomed.users} user
              {doomed.users === 1 ? "" : "s"}, every grant they hold, and every API key
              they own. Nothing is left behind and nothing can be recovered.
            </p>
          ) : (
            <p className="muted">
              No tenant with that id is listed. It may already have been
              deleted.
            </p>
          )}
          {remove.error != null && <ErrorBlock error={remove.error} />}
          <div className="row">
            <Link className="link" to="/tenants">
              cancel
            </Link>
            <span className="spacer" />
            <button
              className="danger"
              disabled={!doomed || remove.isPending}
              onClick={() => doomed && remove.mutate(doomed.id!)}
            >
              {remove.isPending ? "Deleting…" : "Delete permanently"}
            </button>
          </div>
        </div>
      )}

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
              { k: "Known tenants", v: data.registered.length },
            ]}
          />

          {!data.ready && (
            <div className="note warn">
              The data plane did not answer, so the serving counts above are
              zero. The table below is still accurate — it is what the control
              plane knows.
            </div>
          )}

          <p className="muted">
            A tenant is a record in the database, a set of bindings in the
            registry, and a slice of the serving snapshot. Those drift apart in
            ways that are individually invisible, so all three are here:{" "}
            <strong>users</strong> is who can authenticate,{" "}
            <strong>backends</strong> is what the registry binds, and{" "}
            <strong>tools</strong> is what the snapshot actually admits.
          </p>

          <Table
            columns={["Slug", "Name", "Status", "Users", "Backends", "Tools", ""]}
            rows={data.registered.map((t) => [
              <code>{t.slug}</code>,
              t.name,
              <TenantStatus status={t.status} />,
              <Count
                value={t.users}
                warnAtZero={t.id !== undefined}
                why="Nobody can authenticate into this tenant."
              />,
              <Count
                value={t.backends}
                warnAtZero
                why="Nothing is bound, so no tool can reach this tenant whatever its users are granted."
              />,
              <Count value={t.tools} />,
              t.id ? (
                <>
                  <Link className="link" to={`/tenants/${t.id}/users`}>
                    users →
                  </Link>{" "}
                  <Link className="link" to={`/tenants/${t.id}/delete`}>
                    delete
                  </Link>
                </>
              ) : (
                <span className="muted">registry only</span>
              ),
            ])}
            empty="No tenants. Create one, then add a user to it."
          />

          {unregistered.length > 0 && (
            <div className="note warn">
              <strong>
                Bound by the registry, but no tenant record exists:{" "}
                {unregistered.join(", ")}.
              </strong>{" "}
              Nobody can authenticate into{" "}
              {unregistered.length === 1 ? "it" : "them"}. Create the tenant, or
              remove the binding from the registry.
            </div>
          )}
          {unbound.length > 0 && (
            <div className="note warn">
              <strong>No backend bindings: {unbound.join(", ")}.</strong> Their
              users can sign in and will see an empty catalog. Bind a backend to{" "}
              {unbound.length === 1 ? "it" : "them"} in the registry.
            </div>
          )}
        </>
      )}
    </Screen>
  );
}

/**
 * A count that flags itself when zero means something is wrong.
 *
 * The warning belongs on the row rather than only in a banner underneath: a
 * banner naming a condition leaves the reader scanning for the row it applies
 * to, which is exactly the work the table should have already done.
 */
function Count({
  value,
  warnAtZero,
  why,
}: {
  value: number;
  warnAtZero?: boolean;
  why?: string;
}) {
  if (warnAtZero && value === 0) {
    return (
      <span className="mono count-warn" title={why}>
        ⚠ 0
      </span>
    );
  }
  return <span className="mono">{value}</span>;
}

function TenantStatus({ status }: { status: string }) {
  if (status === "unregistered") {
    return <span className="badge badge-bad">unregistered</span>;
  }
  if (status === "active") return <span className="badge badge-ok">active</span>;
  return <span className="badge">{status}</span>;
}
