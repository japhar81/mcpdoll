import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  BrowserRouter,
  NavLink,
  Navigate,
  Route,
  Routes,
} from "react-router-dom";

import { ROUTES, SECTIONS } from "./routes.tsx";
import { AuthProvider, useAuth } from "./lib/auth.tsx";
import { IdentityProvider } from "./lib/identity.tsx";
import { RequireAuth } from "./components/RequireAuth.tsx";
import { LoginScreen } from "./screens/LoginScreen.tsx";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: {
    // Failing visibly beats retrying invisibly: a 403 is a state to show, not a
    // transient to paper over. A 401 no longer reaches a screen at all — the
    // auth provider catches it and sends the user to sign in.
    queries: { retry: false, refetchOnWindowFocus: false },
  },
});

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <AuthProvider>
          <IdentityProvider>
            <Routes>
              {/* Outside the shell and outside the guard, or signing in would
                  require being signed in. */}
              <Route path="/login" element={<LoginScreen />} />
              <Route
                path="*"
                element={
                  <RequireAuth>
                    <Console />
                  </RequireAuth>
                }
              />
            </Routes>
          </IdentityProvider>
        </AuthProvider>
      </BrowserRouter>
    </QueryClientProvider>
  );
}

function Console() {
  return (
    <div className="app-shell">
      <Sidebar />
      <Routes>
        <Route path="/" element={<Navigate to="/overview" replace />} />
        {ROUTES.map((r) => (
          <Route key={r.path} path={r.path} element={<r.component />} />
        ))}
        <Route path="*" element={<NotFound />} />
      </Routes>
    </div>
  );
}

function Sidebar() {
  const auth = useAuth();

  return (
    <aside className="sidebar">
      <h1>MCPDoll</h1>
      <p className="tagline">MCP gateway console</p>

      {SECTIONS.map((section) => {
        const items = ROUTES.filter((r) => r.section === section && r.nav);
        if (items.length === 0) return null;
        return (
          <div key={section}>
            <div className="section">{section}</div>
            <nav>
              {items.map((r) => (
                <NavLink
                  key={r.path}
                  to={r.path}
                  end
                  className={({ isActive }) => (isActive ? "active" : "")}
                >
                  {r.nav}
                </NavLink>
              ))}
            </nav>
          </div>
        );
      })}

      <div className="conn">
        {auth.status === "anonymous" ? (
          <>
            {/* Not a footnote. An unauthenticated control plane can mint a
                signing key, and whoever is looking at this should know. */}
            <div className="conn-state conn-state--warn">Unauthenticated</div>
            <p className="conn-detail">
              This control plane was started with <code>--allow-anonymous</code>
              . Anyone who can reach it can publish.
            </p>
          </>
        ) : (
          <>
            <div className="conn-state">Signed in</div>
            <button
              className="conn-signout"
              type="button"
              onClick={auth.signOut}
            >
              Sign out
            </button>
          </>
        )}
      </div>
    </aside>
  );
}

function NotFound() {
  return (
    <section className="screen">
      <header className="toolbar">
        <strong>Not found</strong>
      </header>
      <div className="screen-body">
        <p className="muted">No console route matches this URL.</p>
      </div>
    </section>
  );
}
