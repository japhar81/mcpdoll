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
            The slug appears verbatim in every scope string this tenant&apos;s
            grants use, so it cannot be changed afterwards — renaming would
            orphan every grant naming it.
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
              {doomed.users === 1 ? "" : "s"}, every grant they hold, and every
              API key they own. The cascade is the schema&apos;s, so nothing is
              left behind — and nothing can be recovered.
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
              <span className="mono">{t.users}</span>,
              <span className="mono">{t.backends}</span>,
              <span className="mono">{t.tools}</span>,
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

          {data.registered.some((t) => t.status === "unregistered") && (
            <div className="note warn">
              <strong>Some slugs are bound but do not exist.</strong> The
              registry routes to them, but no tenant record does, so nobody can
              authenticate into them and their tools reach no one.
            </div>
          )}
          {data.registered.some((t) => t.id && t.backends === 0) && (
            <div className="note warn">
              <strong>Some tenants have no backend bindings.</strong> Their
              users can sign in and will see an empty catalog, whatever they are
              granted — there is nothing bound for them to reach.
            </div>
          )}
        </>
      )}
    </Screen>
  );
}

function TenantStatus({ status }: { status: string }) {
  if (status === "unregistered") {
    return <span className="badge badge-bad">unregistered</span>;
  }
  if (status === "active") return <span className="badge badge-ok">active</span>;
  return <span className="badge">{status}</span>;
}
