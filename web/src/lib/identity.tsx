import { createContext, useContext, useState, type ReactNode } from "react";

/**
 * The identity being inspected, shared across the gateway screens.
 *
 * It lives in React state rather than in the URL deliberately. A subject is a
 * person's identifier, and putting it in the query string writes it into
 * browser history, the referrer of anything the page links to, and any proxy's
 * access log. The API takes it as a query parameter because that request is
 * point-to-point; the console's own URL is not.
 *
 * Sharing it also fixes a small cruelty: inspecting a catalog as someone and
 * then clicking through to call a tool used to mean retyping who you were.
 */
export interface IdentityValue {
  subject: string;
  /** Comma-separated, as typed. Split at the edge, not here. */
  groups: string;
}

interface IdentityContextValue {
  identity: IdentityValue;
  setIdentity: (next: IdentityValue) => void;
}

const IdentityContext = createContext<IdentityContextValue | null>(null);

export function IdentityProvider({ children }: { children: ReactNode }) {
  const [identity, setIdentity] = useState<IdentityValue>({
    subject: "",
    groups: "",
  });
  return (
    <IdentityContext.Provider value={{ identity, setIdentity }}>
      {children}
    </IdentityContext.Provider>
  );
}

export function useIdentity(): IdentityContextValue {
  const ctx = useContext(IdentityContext);
  if (!ctx) throw new Error("useIdentity used outside IdentityProvider");
  return ctx;
}

export function toGroupList(groups: string): string[] {
  return groups
    .split(",")
    .map((g) => g.trim())
    .filter(Boolean);
}
