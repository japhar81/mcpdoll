import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";

import { createUser, listUsers } from "../lib/api.ts";
import { ErrorBlock, Screen, Table } from "../components/Screen.tsx";

/**
 * One tenant's users, and the form that adds one.
 *
 * A new user holds nothing and therefore sees nothing. That is deliberate and
 * the screen says so: an account that could reach tools the moment it existed
 * would make onboarding the thing that grants access.
 */
export function UsersScreen() {
  const { tenantId = "" } = useParams();
  const { pathname } = useLocation();
  const adding = pathname.endsWith("/new");

  const client = useQueryClient();
  const navigate = useNavigate();
  const q = useQuery({
    queryKey: ["users", tenantId],
    queryFn: () => listUsers(tenantId),
    enabled: tenantId !== "",
  });

  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");

  const add = useMutation({
    mutationFn: () =>
      createUser(tenantId, {
        email: email.trim(),
        display_name: name.trim() || undefined,
        password: password || undefined,
      }),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["users", tenantId] });
      setEmail("");
      setName("");
      setPassword("");
      navigate(`/tenants/${tenantId}/users`);
    },
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
          {!adding && (
            <>
              {" · "}
              <Link className="link" to={`/tenants/${tenantId}/users/new`}>
                new user
              </Link>
            </>
          )}
        </>
      }
    >
      {adding && (
        <div className="card">
          <div className="card-head">
            <strong>New user</strong>
          </div>
          <p className="muted">
            The password is optional. A user who signs in through an identity
            provider has none, and a service identity that only ever holds API
            keys does not need one.
          </p>
          <label className="field">
            Email
            <input
              type="email"
              spellCheck={false}
              placeholder="alice@example.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </label>
          <label className="field">
            Display name
            <input value={name} onChange={(e) => setName(e.target.value)} />
          </label>
          <label className="field">
            Password (optional)
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          {add.error != null && <ErrorBlock error={add.error} />}
          <div className="row">
            <Link className="link" to={`/tenants/${tenantId}/users`}>
              cancel
            </Link>
            <span className="spacer" />
            <button
              className="primary"
              disabled={!email.trim() || add.isPending}
              onClick={() => add.mutate()}
            >
              {add.isPending ? "Creating…" : "Create"}
            </button>
          </div>
        </div>
      )}

      {q.data && (
        <>
          <p className="muted">
            A new user holds no grants and sees no tools. Grants are what shapes
            a catalog, so an account is not usable until somebody issues one.
          </p>
          <Table
            columns={["Email", "Name", "Status", "Local password", ""]}
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
              u.has_password ? (
                "yes"
              ) : (
                <span className="muted">no — SSO or key-only</span>
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
            empty="No users in this tenant. Nothing can authenticate into it yet."
          />
        </>
      )}
    </Screen>
  );
}
