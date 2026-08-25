import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";

import { deleteUser, getUser, updateUser } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats } from "../components/Screen.tsx";

/**
 * One user, and the form that changes their status.
 *
 * Disabling is the offboarding path and the screen says what it costs: a key's
 * effective grants are recomputed from its owner at every resolution, so
 * disabling the person stops every credential they hold. That is what makes
 * offboarding one action rather than a hunt through credentials.
 */
export function UserScreen() {
  const { userId = "" } = useParams();
  const { pathname } = useLocation();
  const editing = pathname.endsWith("/edit");
  const deleting = pathname.endsWith("/delete");

  const client = useQueryClient();
  const navigate = useNavigate();
  const q = useQuery({
    queryKey: ["user", userId],
    queryFn: () => getUser(userId),
    enabled: userId !== "",
  });

  const [name, setName] = useState("");
  const [status, setStatus] = useState("active");

  // Seeded from the loaded user rather than from a default, so opening the form
  // and saving without touching anything is a no-op instead of a silent reset.
  useEffect(() => {
    if (q.data) {
      setName(q.data.display_name ?? "");
      setStatus(q.data.status);
    }
  }, [q.data]);

  const remove = useMutation({
    mutationFn: () => deleteUser(userId),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["users"] });
      navigate(user ? `/tenants/${user.tenant_id}/users` : "/tenants");
    },
  });

  const save = useMutation({
    mutationFn: () =>
      updateUser(userId, { display_name: name.trim() || undefined, status }),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["user", userId] });
      client.invalidateQueries({ queryKey: ["users"] });
      navigate(`/users/${userId}`);
    },
  });

  const user = q.data;

  return (
    <Screen
      title={user ? user.email : "User"}
      isLoading={q.isLoading}
      error={q.error}
      actions={
        user && (
          <>
            <Link className="link" to={`/tenants/${user.tenant_id}/users`}>
              ← {user.tenant}
            </Link>
            {" · "}
            <Link className="link" to={`/users/${userId}/grants`}>
              grants
            </Link>
            {" · "}
            <Link className="link" to={`/users/${userId}/keys`}>
              keys
            </Link>
            {!editing && !deleting && (
              <>
                {" · "}
                <Link className="link" to={`/users/${userId}/edit`}>
                  edit
                </Link>
                {" · "}
                <Link className="link" to={`/users/${userId}/delete`}>
                  delete
                </Link>
              </>
            )}
          </>
        )
      }
    >
      {user && (
        <Stats
          items={[
            { k: "Tenant", v: <code>{user.tenant}</code>, small: true },
            {
              k: "Status",
              v:
                user.status === "active" ? (
                  <span className="badge badge-ok">active</span>
                ) : (
                  <span className="badge badge-bad">disabled</span>
                ),
            },
            { k: "Local password", v: user.has_password ? "yes" : "no" },
            { k: "Created", v: user.created_at.slice(0, 10), small: true },
          ]}
        />
      )}

      {user && user.status === "disabled" && (
        <div className="note warn">
          <strong>This account is disabled.</strong> Every API key it owns stops
          resolving, because a key&apos;s effective grants are recomputed from
          its owner at every resolution.
        </div>
      )}

      {deleting && user && (
        <div className="card">
          <div className="card-head">
            <strong>Delete {user.email}?</strong>
            <span className="badge badge-bad">irreversible</span>
          </div>
          <p className="muted">
            Deletes every grant and API key they hold. Their credentials stop
            resolving within seconds.
          </p>
          <p className="muted">
            To offboard somebody, disable them instead — the row stays, so an
            audit can still answer who did what. Delete is for an account that
            should never have existed.
          </p>
          {remove.error != null && <ErrorBlock error={remove.error} />}
          <div className="row">
            <Link className="link" to={`/users/${userId}`}>
              cancel
            </Link>
            <span className="spacer" />
            <button
              className="danger"
              disabled={remove.isPending}
              onClick={() => remove.mutate()}
            >
              {remove.isPending ? "Deleting…" : "Delete permanently"}
            </button>
          </div>
        </div>
      )}

      {editing && user && (
        <div className="card">
          <div className="card-head">
            <strong>Edit {user.email}</strong>
          </div>
          <label className="field">
            Display name
            <input value={name} onChange={(e) => setName(e.target.value)} />
          </label>
          <label className="field">
            Status
            <select value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="active">active</option>
              <option value="disabled">disabled</option>
            </select>
          </label>
          {status === "disabled" && user.status !== "disabled" && (
            <p className="muted">
              Disabling stops every key this user holds — not only their
              password sign-in. The keys stay listed so an incident review can
              still see them.
            </p>
          )}
          {save.error != null && <ErrorBlock error={save.error} />}
          <div className="row">
            <Link className="link" to={`/users/${userId}`}>
              cancel
            </Link>
            <span className="spacer" />
            <button
              className="primary"
              disabled={save.isPending}
              onClick={() => save.mutate()}
            >
              {save.isPending ? "Saving…" : "Save"}
            </button>
          </div>
        </div>
      )}
    </Screen>
  );
}
