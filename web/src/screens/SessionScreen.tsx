import { useQuery } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";

import { getSession } from "../lib/api.ts";
import { useAuth } from "../lib/auth.tsx";
import { Screen, Stats, Table } from "../components/Screen.tsx";

/**
 * Who you are, and what that lets you do.
 *
 * The fastest way to tell which of three credentials is actually in play. A
 * `static` kind means the deployment's break-glass token, which holds every
 * permission — worth knowing before concluding that a check passed for a
 * reason (ADR 0022).
 */
export function SessionScreen() {
  const navigate = useNavigate();
  const auth = useAuth();
  const q = useQuery({ queryKey: ["session"], queryFn: getSession });
  const me = q.data;

  return (
    <Screen
      title="Your credential"
      isLoading={q.isLoading}
      error={q.error}
      actions={
        <button
          className="secondary"
          onClick={() => {
            auth.signOut();
            navigate("/login", { replace: true });
          }}
        >
          Sign out
        </button>
      }
    >
      {me && (
        <>
          <Stats
            items={[
              { k: "Signed in as", v: me.subject, small: true },
              { k: "Kind", v: <CredentialKind kind={me.kind} /> },
              { k: "Tenant", v: me.tenant || "—", small: true },
              { k: "Grants", v: me.grants.length },
            ]}
          />

          {me.kind === "static" && (
            <div className="note warn">
              <strong>This is the deployment&apos;s break-glass token.</strong>{" "}
              It holds every permission, including minting a signing key that
              every data-plane instance trusts. It exists so CI can build a
              snapshot before any user does, and so a control plane whose
              database is down is still inspectable. Every use of it is logged.
              <br />
              Sign in as a person for ordinary work.
            </div>
          )}

          <h2>Grants</h2>
          <p className="muted">
            The same grants that decide what an agent sees through the gateway.
            One authorization model, not two — which is what stops the control
            plane and the data plane drifting into different answers.
          </p>
          <Table
            columns={["Role", "Scope"]}
            rows={me.grants.map((g) => [
              <code>{g.role}</code>,
              <code>{g.scope}</code>,
            ])}
            empty="No grants. You can sign in and see nothing, which is the correct state for an account nobody has granted anything yet."
          />

          <h2>Permissions at global scope</h2>
          <p className="muted">
            Global scope only. A tenant administrator holds theirs at{" "}
            <code>t/&lt;tenant&gt;</code> and this list is empty for them —
            flattening the union would claim more than they have.
          </p>
          {me.permissions.length > 0 ? (
            <div className="chips">
              {me.permissions.map((p) => (
                <code key={p}>{p}</code>
              ))}
            </div>
          ) : (
            <p className="muted">
              None. Anything you hold is scoped to a tenant, which is the normal
              shape for a tenant administrator.
            </p>
          )}
        </>
      )}
    </Screen>
  );
}

function CredentialKind({ kind }: { kind: string }) {
  if (kind === "static") {
    return <span className="badge badge-bad">deployment token</span>;
  }
  if (kind === "api_key") return <span className="badge badge-write">API key</span>;
  return <span className="badge badge-ok">session</span>;
}
