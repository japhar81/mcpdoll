import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";

import { useAuth } from "../lib/auth.tsx";

/**
 * Sign in to the control plane.
 *
 * It asks for a bearer token rather than a username and password, and says so.
 * There is no identity provider in this build (see docs/deferred.md), and a
 * form that said "Password" would be lying about what it wants and about what
 * the credential can do — a control-plane token can mint a signing key.
 */
export function LoginScreen() {
  const auth = useAuth();
  const navigate = useNavigate();
  const location = useLocation();

  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Where the user was headed before being sent here, so signing in resumes
  // rather than dumping them on the front page.
  const from =
    (location.state as { from?: string } | null)?.from ?? "/overview";

  if (auth.status === "authenticated" || auth.status === "anonymous") {
    return <Navigate to={from} replace />;
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);

    const result = await auth.signIn(token);
    if (result === "ok") {
      navigate(from, { replace: true });
      return;
    }
    setError(
      result === "unauthorized"
        ? "The control plane did not accept that token."
        : "The control plane could not be reached. Is it running on :3001?",
    );
    setBusy(false);
  }

  return (
    <div className="login-shell">
      <form className="login-card" onSubmit={submit}>
        <div className="login-brand">
          <span className="login-logo">◆</span>
          <h1>MCPDoll</h1>
        </div>
        <p className="login-sub">Sign in to the control plane</p>

        <div className="login-form">
          <label>
            API token
            <input
              type="password"
              autoComplete="current-password"
              autoFocus
              spellCheck={false}
              placeholder="bearer token"
              value={token}
              onChange={(e) => setToken(e.target.value)}
            />
          </label>

          {error && <div className="login-error">{error}</div>}

          <button
            className="primary login-submit"
            type="submit"
            disabled={busy}
          >
            {busy ? "Checking…" : "Sign in"}
          </button>
        </div>

        <div className="login-note">
          <p>
            The control plane refuses to start without a token unless it was
            given <code>--allow-anonymous</code>. If it was, leave this blank
            and sign in.
          </p>
          <p>
            This token can rebuild the serving snapshot and mint a signing key.
            It is not a read-only credential.
          </p>
        </div>
      </form>
    </div>
  );
}
