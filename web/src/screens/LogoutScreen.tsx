import { useEffect } from "react";
import { Navigate, useNavigate } from "react-router-dom";

import { useAuth } from "../lib/auth.tsx";

/**
 * Sign out, and say what that actually did.
 *
 * A route rather than only a button, because signing out is a real operation
 * with a server side: the session is revoked and a revocation list is
 * published, so it stops working at once rather than at its expiry. A console
 * that only forgot the token locally would leave a live credential behind.
 */
export function LogoutScreen() {
  const auth = useAuth();
  const navigate = useNavigate();

  useEffect(() => {
    auth.signOut();
    navigate("/login", { replace: true });
  }, [auth, navigate]);

  if (auth.status === "signed-out") {
    return <Navigate to="/login" replace />;
  }
  return <p className="muted">Signing out…</p>;
}
