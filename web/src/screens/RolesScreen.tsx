import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { deleteRole, listRoles, putRole } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats, Table } from "../components/Screen.tsx";
import type { Role } from "../lib/types.ts";

/**
 * The role catalog, composed from the closed permission set.
 *
 * The distinction this screen turns on: the permission *vocabulary* is closed,
 * because a set that grows casually stops being reviewable — adding one is a
 * schema and code change. Composing roles out of it is ordinary administration,
 * and used to be a code change for no good reason (ADR 0028).
 *
 * What stops this from being an escalation console: you cannot put a permission
 * in a role that you do not hold, and — the rule that actually matters — you
 * cannot grant a role conferring a permission you lack at that scope.
 */
export function RolesScreen() {
  const q = useQuery({ queryKey: ["roles"], queryFn: listRoles });
  const [editing, setEditing] = useState<string | null>(null);

  // Permissions no role grants: an operation nobody in the deployment can
  // reach. Not visible from the table below, which is why it is called out.
  const granted = new Set((q.data?.roles ?? []).flatMap((r) => r.permissions));
  const unreachable = (q.data?.permissions ?? []).filter((p) => !granted.has(p));

  return (
    <Screen
      title="Roles"
      isLoading={q.isLoading}
      error={q.error}
      actions={
        <button onClick={() => setEditing("")} disabled={editing !== null}>
          New role
        </button>
      }
    >
      {q.data && (
        <>
          <Stats
            items={[
              { k: "roles", v: q.data.roles.length },
              { k: "permissions", v: q.data.permissions.length },
              { k: "granted by no role", v: unreachable.length },
            ]}
          />

          {editing !== null && (
            <RoleEditor
              all={q.data.permissions}
              role={q.data.roles.find((r) => r.name === editing)}
              onDone={() => setEditing(null)}
            />
          )}

          <Table
            columns={["Role", "Permissions", ""]}
            rows={q.data.roles.map((r) => [
              <>
                {r.name}
                {r.builtin && <span className="pill muted">built in</span>}
                {r.description && (
                  <>
                    <br />
                    <span className="muted">{r.description}</span>
                  </>
                )}
              </>,
              r.permissions.length ? (
                <span className="chips">
                  {r.permissions.map((p) => (
                    <code key={p}>{p}</code>
                  ))}
                </span>
              ) : (
                <span className="muted">grants nothing</span>
              ),
              <RoleActions role={r} onEdit={() => setEditing(r.name)} />,
            ])}
          />

          {unreachable.length > 0 && (
            <div className="note warn">
              <strong>No role grants these.</strong> Nobody in this deployment
              can reach the operations behind them:{" "}
              {unreachable.map((p) => (
                <code key={p}>{p}</code>
              ))}
            </div>
          )}
        </>
      )}
    </Screen>
  );
}

function RoleActions(props: { role: Role; onEdit: () => void }) {
  const client = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const del = useMutation({
    mutationFn: () => deleteRole(props.role.name),
    onSuccess: () => void client.invalidateQueries({ queryKey: ["roles"] }),
  });

  return (
    <>
      <button onClick={props.onEdit}>Edit</button>{" "}
      {/* A built-in has no delete control at all rather than one that is
          always refused: the seed recreates it on the next boot, so there is
          no state in which the button would work. */}
      {!props.role.builtin &&
        (confirming ? (
          <>
            <button className="danger" onClick={() => del.mutate()}>
              Really delete
            </button>{" "}
            <button onClick={() => setConfirming(false)}>Cancel</button>
          </>
        ) : (
          <button onClick={() => setConfirming(true)}>Delete</button>
        ))}
      {del.error != null && <ErrorBlock error={del.error} />}
    </>
  );
}

function RoleEditor(props: {
  all: string[];
  role?: Role;
  onDone: () => void;
}) {
  const client = useQueryClient();
  const [name, setName] = useState(props.role?.name ?? "");
  const [description, setDescription] = useState(props.role?.description ?? "");
  const [chosen, setChosen] = useState<string[]>(props.role?.permissions ?? []);

  const save = useMutation({
    mutationFn: () =>
      putRole(name.trim(), { description, permissions: chosen }),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["roles"] });
      props.onDone();
    },
  });

  const toggle = (p: string) =>
    setChosen((was) =>
      was.includes(p) ? was.filter((x) => x !== p) : [...was, p],
    );

  return (
    <div className="card">
      <div className="card-head">
        <strong>{props.role ? `Edit ${props.role.name}` : "New role"}</strong>
      </div>

      <label className="field">
        Name
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          disabled={props.role !== undefined}
          placeholder="release_manager"
        />
      </label>
      <label className="field">
        What it is for
        <input
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </label>

      <p className="muted">
        The complete set, not a change to it: what is ticked here is what the
        role permits when you save. A role with nothing ticked grants nothing,
        which is how you neutralize one without deleting it and breaking the
        grants that name it.
      </p>
      <div className="checks">
        {props.all.map((p) => (
          <label key={p} className="inline">
            <input
              type="checkbox"
              checked={chosen.includes(p)}
              onChange={() => toggle(p)}
            />
            <code>{p}</code>
          </label>
        ))}
      </div>

      {save.error != null && <ErrorBlock error={save.error} />}
      <button
        className="primary"
        disabled={!name.trim() || save.isPending}
        onClick={() => save.mutate()}
      >
        {save.isPending ? "Saving…" : "Save"}
      </button>{" "}
      <button onClick={props.onDone}>Cancel</button>
    </div>
  );
}
