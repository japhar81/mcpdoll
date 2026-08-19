import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";

import { useAuth } from "../lib/auth.tsx";

/**
 * Gate the console behind a working credential.
 *
 * The redirect happens *before* any screen renders, so an unauthenticated visit
 * never fires a request that 401s. The alternative — render, fail, show an
 * error — is what this replaces: it made every screen responsible for
 * explaining an authentication problem, and none of them could offer the fix.
 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const auth = useAuth();
  const location = useLocation();

  if (auth.status === "checking") {
    // A blank hold rather than a spinner. The check is one request against a
    // static list; a spinner would flash for 30ms and read as jank.
    return <div className="auth-checking" />;
  }

  if (auth.status === "signed-out") {
    // Carry the destination so signing in resumes rather than restarting.
    return (
      <Navigate
        to="/login"
        replace
        state={{ from: location.pathname + location.search }}
      />
    );
  }

  return <>{children}</>;
}
