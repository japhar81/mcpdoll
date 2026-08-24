import {
  createContext,
  useContext,
  useState,
  type ReactNode,
} from "react";

/**
 * The credential being inspected, shared across the gateway screens.
 *
 * A credential rather than a subject and a group list. With one endpoint and
 * per-principal catalogs (ADR 0019), the only trustworthy way to see what a
 * principal sees is to present what they present — claiming a subject would be
 * re-deriving policy, which is the mistake the inspector exists to avoid.
 *
 * Held in React state and never persisted. It is somebody else's agent
 * credential: it should not outlive the tab, and it must not reach
 * localStorage, browser history, or a referrer.
 */
interface InspectionContextValue {
  credential: string;
  setCredential: (next: string) => void;
}

const InspectionContext = createContext<InspectionContextValue | null>(null);

export function InspectionProvider({ children }: { children: ReactNode }) {
  const [credential, setCredential] = useState("");
  return (
    <InspectionContext.Provider value={{ credential, setCredential }}>
      {children}
    </InspectionContext.Provider>
  );
}

export function useInspection(): InspectionContextValue {
  const ctx = useContext(InspectionContext);
  if (!ctx) throw new Error("useInspection used outside InspectionProvider");
  return ctx;
}
