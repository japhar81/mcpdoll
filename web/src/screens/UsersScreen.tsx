import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";

import { listUsers } from "../lib/api.ts";
import { Screen, Table } from "../components/Screen.tsx";

/**
 * The users granted into one tenant.
 *
 * Granted into, not owned by — a user belongs to no tenant. This answers "who
 * can reach this tenant", which is the question ownership only ever
 * approximated. Creating a person happens on the global Users screen; a grant
 * is what puts them here.
 */
export function UsersScreen() {
  const { tenantId = "" } = useParams();
  const q = useQuery({
    queryKey: ["users", tenantId],
    queryFn: () => listUsers(tenantId),
    enabled: tenantId !== "",
  });

  return (
    <Screen
      title={q.data ? `Users in ${q.data.tenant}` : "Users"}
      isLoading={q.isLoading}
      error={q.error}
      actions={
        <>
          <Link className="link" to="/tenants">
            ← tenants
          </Link>
          {" · "}
          <Link className="link" to="/users">
            all users
          </Link>
        </>
      }
    >
      {q.data && (
        <>
          <p className="muted">
            Users whose grants reach this tenant. A grant at global scope reaches
            every tenant, so a platform administrator appears in all of them.
          </p>
          <Table
            columns={["Email", "Name", "Status", ""]}
            rows={q.data.users.map((u) => [
              <Link className="link" to={`/users/${u.id}`}>
                {u.email}
              </Link>,
              u.display_name ?? <span className="muted">—</span>,
              u.status === "active" ? (
                <span className="badge badge-ok">active</span>
              ) : (
                <span className="badge badge-bad">disabled</span>
              ),
              <>
                <Link className="link" to={`/users/${u.id}/grants`}>
                  grants
                </Link>{" "}
                <Link className="link" to={`/users/${u.id}/keys`}>
                  keys
                </Link>
              </>,
            ])}
            empty="Nobody is granted into this tenant, so nothing can authenticate into it."
          />
        </>
      )}
    </Screen>
  );
}
