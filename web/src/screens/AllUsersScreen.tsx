import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate } from "react-router-dom";

import { createUser, listAllUsers } from "../lib/api.ts";
import { ErrorBlock, Screen, Table } from "../components/Screen.tsx";

/**
 * Everybody, across the whole install.
 *
 * A user is a person and belongs to no tenant — which tenants they reach is
 * what their grants say. A tenant's page lists the users granted into it, which
 * is a different and narrower question.
 */
export function AllUsersScreen() {
  const { pathname } = useLocation();
  const adding = pathname.endsWith("/new");

  const client = useQueryClient();
  const navigate = useNavigate();
  const q = useQuery({ queryKey: ["users"], queryFn: listAllUsers });

  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");

  const add = useMutation({
    mutationFn: () =>
      createUser({
        email: email.trim(),
        display_name: name.trim() || undefined,
        password: password || undefined,
      }),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["users"] });
      setEmail("");
      setName("");
      setPassword("");
      navigate("/users");
    },
  });

  return (
    <Screen
      title="Users"
      isLoading={q.isLoading}
      error={q.error}
      actions={
        !adding && (
          <Link className="link" to="/users/new">
            new user
          </Link>
        )
      }
    >
      {adding && (
        <div className="card">
          <div className="card-head">
            <strong>New user</strong>
          </div>
          <p className="muted">
            Creating a user grants nothing. They reach a tenant when you grant
            them something in it.
          </p>
          <p className="muted">
            The password is optional — an identity-provider user has none, and a
            service identity that only holds API keys does not need one.
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
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>
          {add.error != null && <ErrorBlock error={add.error} />}
          <div className="row">
            <Link className="link" to="/users">
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
          empty="No users. Create one, then grant them something."
        />
      )}
    </Screen>
  );
}
