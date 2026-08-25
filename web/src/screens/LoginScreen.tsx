import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";

import { useAuth } from "../lib/auth.tsx";

/**
 * Sign in to the control plane. Email and password, nothing else.
 *
 * This used to offer the deployment token as a second form. That was
 * scaffolding I kept: before sessions existed the console *had* to hold that
 * token, and when sessions arrived I left the old path on the screen and wrote
 * a justification for it. There is no console use for it — CI builds snapshots
 * through the CLI, and a console whose database is unreachable can show nothing
 * anyway. It remains an API and CLI credential; it is not a way to sign in
 * here.
 */
export function LoginScreen() {
  const auth = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  const [tenant, setTenant] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [passwordError, setPasswordError] = useState<string | null>(null);

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

      </div>
    </div>
  );
}
