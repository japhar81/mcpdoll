import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import {
  getSession,
  login,
  logout,
  probeCredential,
  setToken,
  setUnauthorizedHandler,
} from "./api.ts";
import type { SessionInfo } from "./types.ts";

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
  /**
   * Who the control plane says the caller is, and what they may do.
   *
   * Null while checking, and after an anonymous start where there is nobody to
   * be. Screens render from this: a button that 403s is worse than a button
   * that is not there.
   */
  session: SessionInfo | null;
  /** Sign in as a person. Returns the reason on failure. */
  signInWithPassword: (
    email: string,
    password: string,
  ) => Promise<"ok" | "unauthorized" | "unreachable">;
  /** Adopt a raw credential — an API key, or the deployment token. */
  signIn: (token: string) => Promise<"ok" | "unauthorized" | "unreachable">;
  signOut: () => void;
  /** Does the caller hold this permission at global scope? */
  can: (permission: string) => boolean;
}

const AuthContext = createContext<AuthContextValue | null>(null);

const TOKEN_KEY = "mcpdoll.token";

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>("checking");
  const [token, setTokenState] = useState("");
  const [session, setSession] = useState<SessionInfo | null>(null);

  // Asked after every successful adoption rather than derived from the
  // credential's shape. The console cannot know what a token may do; only the
  // control plane can, and guessing would mean rendering a button that 403s.
  const loadSession = useCallback(async () => {
    try {
      setSession(await getSession());
    } catch {
      // A control plane too old to answer, or a transient failure. Rendering
      // as if nothing is permitted would be worse than rendering everything
      // and letting the server refuse — the server is the authority either way.
      setSession(null);
    }
  }, []);

  const adopt = useCallback((next: string, nextStatus: AuthStatus) => {
    setToken(next);
    setTokenState(next);
    setStatus(nextStatus);
  }, []);

  const signOut = useCallback(() => {
    // Tell the server first: a session that is only forgotten locally is still
    // a live credential, and the whole point of ADR 0023 is that revocation
    // does not wait. A failure here still signs the user out of this browser.
    void logout().catch(() => undefined);
    localStorage.removeItem(TOKEN_KEY);
    setSession(null);
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
        void loadSession();
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

  const signInWithPassword = useCallback(
    async (email: string, password: string) => {
      try {
        const result = await login(email, password);
        localStorage.setItem(TOKEN_KEY, result.token);
        adopt(result.token, "authenticated");
        await loadSession();
        return "ok" as const;
      } catch (e) {
        // 401 is a wrong credential; status 0 is a control plane that did not
        // answer. Reporting the second as the first sends somebody to reset a
        // password that is fine.
        const status = (e as { status?: number }).status ?? 0;
        return status === 0 ? ("unreachable" as const) : ("unauthorized" as const);
      }
    },
    [adopt, loadSession],
  );

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
      await loadSession();
      return "ok" as const;
    },
    [adopt, loadSession],
  );

  const can = useCallback(
    (permission: string) => {
      // No session means the control plane could not say. Permissive, because
      // the server refuses regardless and hiding everything would make a
      // transient failure look like a lost account.
      if (!session) return true;
      return session.permissions.includes(permission);
    },
    [session],
  );

  const value = useMemo<AuthContextValue>(
    () => ({ status, token, session, signInWithPassword, signIn, signOut, can }),
    [status, token, session, signInWithPassword, signIn, signOut, can],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth used outside AuthProvider");
  return ctx;
}
