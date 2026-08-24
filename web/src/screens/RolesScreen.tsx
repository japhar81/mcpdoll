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
            A grant pairs one of these roles with a scope. The role decides
            what; the scope decides where. <code>tool:list</code> and{" "}
            <code>tool:call</code> are separate on purpose — a role that could
            call without listing would let an agent reach a tool it was never
            shown.
          </p>

          <Table
            columns={["Role", "Permissions"]}
            rows={q.data.roles.map((r) => [
              <code>{r.name}</code>,
              <span className="muted">{r.permissions.join(", ")}</span>,
            ])}
            empty="No roles defined, so no grant can authorize anything."
          />

          <h2>Every permission that exists</h2>
          <p className="muted">
            The complete closed set, not only the ones some role happens to use.
            Adding one is a schema change and a change to this console.
          </p>
          <div className="chips">
            {q.data.permissions.map((p) => (
              <code key={p}>{p}</code>
            ))}
          </div>
        </>
      )}
    </Screen>
  );
}
