import { useQuery } from "@tanstack/react-query";

import { listRoles } from "../lib/api.ts";
import { Screen, Stats, Table } from "../components/Screen.tsx";

/**
 * The role catalog and the closed permission set.
 *
 * Read-only, and that is the point: adding a permission is a schema change, a
 * seed migration, and a change here. The friction exists because a permission
 * set which grows casually stops being reviewable, and a console that could add
 * one with a text field would remove exactly that friction.
 */
export function RolesScreen() {
  const q = useQuery({ queryKey: ["roles"], queryFn: listRoles });

  // Permissions no role grants. The full permission list used to be on this
  // screen and said nothing an operator could act on — with the default
  // catalog `platform_admin` holds every one, so the table below already showed
  // them all. This is the part reading the table does not give you: an
  // operation nobody can reach.
  const granted = new Set((q.data?.roles ?? []).flatMap((r) => r.permissions));
  const unreachable = (q.data?.permissions ?? []).filter((p) => !granted.has(p));

  return (
    <Screen title="Roles" isLoading={q.isLoading} error={q.error}>
      {q.data && (
        <>
          <Stats
            items={[
              { k: "Roles", v: q.data.roles.length },
              { k: "Permissions", v: q.data.permissions.length },
            ]}
          />

          <p className="muted">
            A grant pairs one of these roles with a scope: the role decides
            what, the scope decides where.
          </p>

          <Table
            columns={["Role", "Permissions"]}
            rows={q.data.roles.map((r) => [
              <code>{r.name}</code>,
              <span className="muted">{r.permissions.join(", ")}</span>,
            ])}
            empty="No roles defined, so no grant can authorize anything."
          />

          {unreachable.length > 0 && (
            <div className="note warn">
              <strong>
                {unreachable.length} permission
                {unreachable.length === 1 ? "" : "s"} no role grants.
              </strong>{" "}
              Nobody can hold {unreachable.length === 1 ? "it" : "them"}, so the
              operations behind {unreachable.length === 1 ? "it" : "them"} are
              unreachable: {unreachable.join(", ")}.
            </div>
          )}
        </>
      )}
    </Screen>
  );
}
