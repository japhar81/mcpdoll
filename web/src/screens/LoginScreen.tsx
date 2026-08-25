import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";

import { useAuth } from "../lib/auth.tsx";

/**
 * Sign in. Email and password, and nothing else on the page.
 *
 * It carried two explanatory paragraphs. Nobody reads a login screen — they
 * type a password — and what those paragraphs explained was a design that no
 * longer exists: the tenant field they justified is gone.
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

    const result = await auth.signInWithPassword(email.trim(), password);
    if (result === "ok") {
      navigate(from, { replace: true });
      return;
    }
    setPasswordError(
      result === "unauthorized"
        ? "The email or password is wrong."
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
            Email
            <input
              type="email"
              autoComplete="username"
              autoFocus
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
            disabled={busy || !email.trim()}
          >
            {busy ? "Signing in…" : "Sign in"}
          </button>
        </form>
      </div>
    </div>
  );
}
