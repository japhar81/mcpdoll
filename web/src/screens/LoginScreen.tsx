import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";

import { useAuth } from "../lib/auth.tsx";

/**
 * Sign in to the control plane.
 *
 * A password, because a local password is a principal: the control plane
 * resolves it to a user, reads their grants, and checks every operation against
 * them (ADR 0022). This used to ask for the deployment's bearer token, which
 * made every console user the same principal — the most privileged one.
 *
 * That token still works, and the second form is deliberately below the fold
 * and labelled for what it is. It holds every permission, so a console that
 * offered it as an equal choice would make the break-glass credential the
 * convenient one.
 */
export function LoginScreen() {
  const auth = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  const [tenant, setTenant] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState("");
  const [showToken, setShowToken] = useState(false);
  const [busy, setBusy] = useState(false);
  // One error per form. A single shared state renders a token failure under the
  // password fields, which reads as "my password is wrong" when the password
  // was never submitted.
  const [passwordError, setPasswordError] = useState<string | null>(null);
  const [tokenError, setTokenError] = useState<string | null>(null);

  // Where the user was headed before being sent here, so signing in resumes
  // rather than dumping them on the front page.
  const from =
    (location.state as { from?: string } | null)?.from ?? "/overview";

  if (auth.status === "authenticated" || auth.status === "anonymous") {
    return <Navigate to={from} replace />;
  }

  async function submitPassword(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setPasswordError(null);
    setTokenError(null);

    const result = await auth.signInWithPassword(tenant.trim(), email.trim(), password);
    if (result === "ok") {
      navigate(from, { replace: true });
      return;
    }
    setPasswordError(
      result === "unauthorized"
        ? "The tenant, email, or password is wrong."
        : "The control plane could not be reached. Is it running on :3001?",
    );
    setBusy(false);
  }

  async function submitToken(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setPasswordError(null);
    setTokenError(null);

    const result = await auth.signIn(token);
    if (result === "ok") {
      navigate(from, { replace: true });
      return;
    }
    setTokenError(
      result === "unauthorized"
        ? "The control plane did not accept that credential. Check that the " +
            "field holds what you typed — a password manager will fill a saved " +
            "login into it if you let it."
        : "The control plane could not be reached. Is it running on :3001?",
    );
    setBusy(false);
  }

  return (
    <div className="login-shell">
      <div className="login-card">
        <div className="login-brand">
          <span className="login-logo">◆</span>
          <h1>MCPDoll</h1>
        </div>
        <p className="login-sub">Sign in to the control plane</p>

        <form className="login-form" onSubmit={submitPassword}>
          <label>
            Tenant
            <input
              autoFocus
              spellCheck={false}
              placeholder="acme"
              value={tenant}
              onChange={(e) => setTenant(e.target.value)}
            />
          </label>
          <label>
            Email
            <input
              type="email"
              autoComplete="username"
              spellCheck={false}
              placeholder="you@example.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </label>
          <label>
            Password
            <input
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </label>

          {passwordError && <div className="login-error">{passwordError}</div>}

          <button
            className="primary login-submit"
            type="submit"
            disabled={busy || !tenant.trim() || !email.trim()}
          >
            {busy ? "Signing in…" : "Sign in"}
          </button>
        </form>

        <div className="login-note">
          <p>
            The same email in two tenants is two different people, so the tenant is
            part of who you are.
          </p>
          <p>
            What you can do here is decided by your grants — the same ones that
            decide what an agent sees through the gateway.
          </p>
        </div>

        {!showToken ? (
          <button className="link login-alt" onClick={() => setShowToken(true)}>
            Use the deployment token instead
          </button>
        ) : (
          <form className="login-form login-alt-form" onSubmit={submitToken}>
            <label>
              Deployment token or API key
              <input
                type="password"
                spellCheck={false}
                // Not a login, and telling the browser so matters: without
                // this a password manager fills a saved credential into it,
                // the field looks full, and the submission fails for a reason
                // nothing on screen explains.
                autoComplete="new-password"
                name="mcpdoll-deployment-token"
                data-1p-ignore
                data-lpignore="true"
                placeholder="mcpd.… or the configured token"
                value={token}
                onChange={(e) => setToken(e.target.value)}
              />
            </label>
            {tokenError && <div className="login-error">{tokenError}</div>}
            <button className="secondary login-submit" type="submit" disabled={busy}>
              {busy ? "Checking…" : "Sign in with a token"}
            </button>
            <div className="login-note">
              <p>
                <strong>The deployment token holds every permission.</strong> Every
                use of it is logged. An API key works here too and carries only what
                its owner granted it.
              </p>
              <p>
                Leave it blank if the control plane was started with{" "}
                <code>--allow-anonymous</code>.
              </p>
            </div>
          </form>
        )}
      </div>
    </div>
  );
}
