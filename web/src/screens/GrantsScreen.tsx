import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";

import {
  getUser,
  listGrants,
  listRoles,
  putGrants,
} from "../lib/api.ts";
import { ErrorBlock, Screen, Table } from "../components/Screen.tsx";
import type { Grant } from "../lib/types.ts";

/**
 * What one user holds, and the editor that changes it.
 *
 * The editor is declarative: it submits the complete set, so anything removed
 * from the list is revoked. That is deliberate — the question an operator is
 * answering is "what should this person hold", and expressing it as a sequence
 * of add/remove deltas is exactly how a revocation gets forgotten.
 */
export function GrantsScreen() {
  const { userId = "" } = useParams();
  const { pathname } = useLocation();
  const editing = pathname.endsWith("/edit");

  const client = useQueryClient();
  const navigate = useNavigate();

  const user = useQuery({
    queryKey: ["user", userId],
    queryFn: () => getUser(userId),
    enabled: userId !== "",
  });
  const q = useQuery({
    queryKey: ["grants", userId],
    queryFn: () => listGrants(userId),
    enabled: userId !== "",
  });
  const roles = useQuery({ queryKey: ["roles"], queryFn: listRoles });

  const [draft, setDraft] = useState<Grant[]>([]);
  const [role, setRole] = useState("");
  const [scope, setScope] = useState("");

  useEffect(() => {
    if (q.data) setDraft(q.data.grants);
  }, [q.data]);

  // Suggested rather than imposed. A grant may name a toolset or a single tool,
  // and the console does not know the toolset names a snapshot admits, so the
  // scope stays a free-text field with the tenant's prefix offered.
  useEffect(() => {
    if (user.data && scope === "") setScope(`t/${user.data.tenant}`);
  }, [user.data, scope]);

  const save = useMutation({
    mutationFn: () => putGrants(userId, draft),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["grants", userId] });
      navigate(`/users/${userId}/grants`);
    },
  });

  function add() {
    const entry = { role: role.trim(), scope: scope.trim() };
    if (!entry.role || !entry.scope) return;
    if (draft.some((g) => g.role === entry.role && g.scope === entry.scope)) return;
    setDraft([...draft, entry]);
  }

  const shown = editing ? draft : (q.data?.grants ?? []);

  return (
    <Screen
      title={user.data ? `Grants — ${user.data.email}` : "Grants"}
      isLoading={q.isLoading}
      error={q.error}
      actions={
        <>
          <Link className="link" to={`/users/${userId}`}>
            ← user
          </Link>
          {!editing && (
            <>
              {" · "}
              <Link className="link" to={`/users/${userId}/grants/edit`}>
                edit
              </Link>
            </>
          )}
        </>
      }
    >
      <p className="muted">
        These are the grants the user holds <em>directly</em>. An API key&apos;s
        effective grants are the intersection of what the key declares with this
        set, recomputed at every resolution — so a key can narrow what its owner
        holds but never widen it.
      </p>

      <Table
        columns={editing ? ["Role", "Scope", "Covers", ""] : ["Role", "Scope", "Covers"]}
        rows={shown.map((g) => {
          const cells = [
            <code>{g.role}</code>,
            <code>{g.scope}</code>,
            <span className="muted">{describeScope(g.scope)}</span>,
          ];
          if (editing) {
            cells.push(
              <button
                className="danger"
                onClick={() =>
                  setDraft(
                    draft.filter((d) => d.role !== g.role || d.scope !== g.scope),
                  )
                }
              >
                remove
              </button>,
            );
          }
          return cells;
        })}
        empty="No grants. This principal's catalog is empty, which is the correct state for somebody nobody has granted anything yet."
      />

      {editing && (
        <div className="card">
          <div className="card-head">
            <strong>Add a grant</strong>
          </div>
          <div className="row">
            <label className="field">
              Role
              <select value={role} onChange={(e) => setRole(e.target.value)}>
                <option value="">Select a role…</option>
                {(roles.data?.roles ?? []).map((r) => (
                  <option key={r.name} value={r.name}>
                    {r.name}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              Scope
              <input
                spellCheck={false}
                placeholder="t/acme/ts/support"
                value={scope}
                onChange={(e) => setScope(e.target.value)}
              />
            </label>
            <button className="secondary" disabled={!role || !scope} onClick={add}>
              add
            </button>
          </div>
          <p className="muted">
            Scopes nest, and a grant covers everything below it:
            <br />
            <code>*</code> everything · <code>t/acme</code> one tenant ·{" "}
            <code>t/acme/ts/support</code> one toolset ·{" "}
            <code>t/acme/ts/support/crm.lookup</code> one tool
          </p>
          {role && roles.data && (
            <p className="muted">
              <code>{role}</code> permits:{" "}
              {(roles.data.roles.find((r) => r.name === role)?.permissions ?? []).join(
                ", ",
              )}
            </p>
          )}
          {save.error != null && <ErrorBlock error={save.error} />}
          <div className="row">
            <Link className="link" to={`/users/${userId}/grants`}>
              cancel
            </Link>
            <span className="spacer" />
            <button
              className="primary"
              disabled={save.isPending}
              onClick={() => save.mutate()}
            >
              {save.isPending ? "Saving…" : `Save ${draft.length} grant(s)`}
            </button>
          </div>
          <p className="muted">
            Saving submits the whole list. Anything removed above is revoked —
            and takes effect for the data plane at the next snapshot, not
            immediately.
          </p>
        </div>
      )}
    </Screen>
  );
}

/** Plain English for a scope string, so nesting is legible without the ADR. */
function describeScope(scope: string): string {
  if (scope === "*") return "every tenant, every toolset, every tool";
  const parts = scope.split("/");
  if (parts[0] !== "t" || parts.length < 2) return "malformed";
  const tenant = parts[1];
  if (parts.length === 2) return `everything in tenant ${tenant}`;
  if (parts[2] !== "ts") return "malformed";
  if (parts.length === 4) return `toolset ${parts[3]} in ${tenant}`;
  if (parts.length === 5) return `tool ${parts[4]} of ${parts[3]} in ${tenant}`;
  return "malformed";
}
