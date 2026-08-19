import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { probeCredential, setToken, setUnauthorizedHandler } from "./api.ts";

/**
 * Who the console is talking to the control plane as.
 *
 * `anonymous` is a real state, not a missing one: the control plane can be
 * started with `--allow-anonymous`, and when it is, demanding a credential the
 * server does not want would be a login screen nobody can get past.
 */
export type AuthStatus =
  "checking" | "authenticated" | "anonymous" | "signed-out";

export interface AuthContextValue {
  status: AuthStatus;
  /** The token in use. Empty when the server accepts anonymous requests. */
  token: string;
  /** Verify a credential and adopt it. Returns the reason on failure. */
  signIn: (token: string) => Promise<"ok" | "unauthorized" | "unreachable">;
  signOut: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

const TOKEN_KEY = "mcpdoll.token";

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>("checking");
  const [token, setTokenState] = useState("");

  const adopt = useCallback((next: string, nextStatus: AuthStatus) => {
    setToken(next);
    setTokenState(next);
    setStatus(nextStatus);
  }, []);

  const signOut = useCallback(() => {
    localStorage.removeItem(TOKEN_KEY);
    adopt("", "signed-out");
  }, [adopt]);

  // Decide the starting state before rendering anything that would make a
  // request. Rendering first and reacting to the 401 works, but it means every
  // first visit fires a doomed request and flashes an error.
  useEffect(() => {
    let cancelled = false;

    void (async () => {
      const stored = localStorage.getItem(TOKEN_KEY) ?? "";

      // Probe with whatever is stored — including nothing. An empty probe is
      // how the console discovers a control plane running --allow-anonymous,
      // rather than assuming every deployment wants a credential.
      const result = await probeCredential(stored);
      if (cancelled) return;

      if (result === "ok") {
        adopt(stored, stored ? "authenticated" : "anonymous");
        return;
      }

      // A stored credential that no longer works is worse than none: it makes
      // every screen fail in a way that looks like a server problem. Drop it.
      if (stored) localStorage.removeItem(TOKEN_KEY);
      adopt("", "signed-out");
    })();

    return () => {
      cancelled = true;
    };
  }, [adopt]);

  // A credential can stop working mid-session — the control plane restarts with
  // a different token, or somebody rotates it. Land back on the login screen
  // rather than leaving a dozen screens each showing their own error.
  useEffect(() => {
    setUnauthorizedHandler(() => {
      localStorage.removeItem(TOKEN_KEY);
      adopt("", "signed-out");
    });
    return () => setUnauthorizedHandler(null);
  }, [adopt]);

  const signIn = useCallback(
    async (candidate: string) => {
      const trimmed = candidate.trim();
      const result = await probeCredential(trimmed);
      if (result !== "ok") return result;

      // Only persisted once it is known to work. Storing first and validating
      // later is how a typo survives a reload.
      if (trimmed) {
        localStorage.setItem(TOKEN_KEY, trimmed);
        adopt(trimmed, "authenticated");
      } else {
        localStorage.removeItem(TOKEN_KEY);
        adopt("", "anonymous");
      }
      return "ok" as const;
    },
    [adopt],
  );

  const value = useMemo<AuthContextValue>(
    () => ({ status, token, signIn, signOut }),
    [status, token, signIn, signOut],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth used outside AuthProvider");
  return ctx;
}
