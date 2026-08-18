import { useEffect, useState } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import {
  BrowserRouter,
  NavLink,
  Navigate,
  Route,
  Routes,
} from "react-router-dom";

import { ROUTES, SECTIONS } from "./routes.tsx";
import { setToken, getToken } from "./lib/api.ts";
import { IdentityProvider } from "./lib/identity.tsx";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: {
    // Failing visibly beats retrying invisibly: a 401 or a 403 is a state to
    // show, not a transient to paper over.
    queries: { retry: false, refetchOnWindowFocus: false },
  },
});

const TOKEN_KEY = "mcpdoll.token";

export function App() {
  // Kept in localStorage so a reload does not log you out during development.
  // A real deployment gets a session from the identity provider — see
  // docs/deferred.md.
  const [token, setTokenState] = useState(
    () => localStorage.getItem(TOKEN_KEY) ?? "",
  );

  useEffect(() => {
    setToken(token);
    localStorage.setItem(TOKEN_KEY, token);
  }, [token]);

  // The very first render happens before the effect, so seed the client
  // synchronously too — otherwise the first query goes out unauthenticated and
  // 401s for no reason a user could understand.
  if (getToken() !== token) setToken(token);

  return (
    <QueryClientProvider client={queryClient}>
      <IdentityProvider>
        <BrowserRouter>
          <div className="app-shell">
            <aside className="sidebar">
              <h1>MCPDoll</h1>
              <p className="tagline">MCP gateway console</p>

              {SECTIONS.map((section) => {
                const items = ROUTES.filter(
                  (r) => r.section === section && r.nav,
                );
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
                          className={({ isActive }) =>
                            isActive ? "active" : ""
                          }
                        >
                          {r.nav}
                        </NavLink>
                      ))}
                    </nav>
                  </div>
                );
              })}

              <div className="conn">
                API token
                <input
                  type="password"
                  value={token}
                  spellCheck={false}
                  placeholder="bearer token"
                  onChange={(e) => setTokenState(e.target.value)}
                />
              </div>
            </aside>

            <Routes>
              <Route path="/" element={<Navigate to="/registry" replace />} />
              {ROUTES.map((r) => (
                <Route key={r.path} path={r.path} element={<r.component />} />
              ))}
              <Route path="*" element={<NotFound />} />
            </Routes>
          </div>
        </BrowserRouter>
      </IdentityProvider>
    </QueryClientProvider>
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
