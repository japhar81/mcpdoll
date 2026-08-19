import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The dev server proxies /api to the control plane on :3001, so the console is
// same-origin during development and never needs a CORS grant for the common
// path. The API's allowed_origins list still exists for a deployment that
// serves the console from somewhere else.
// The control plane is on localhost when the console runs on the host, and on a
// container name when it runs in compose. One environment variable rather than
// two configs, because a second config would drift and the difference is one
// hostname.
const controlPlane = process.env.MCPDOLL_API_URL ?? "http://localhost:3001";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: controlPlane, changeOrigin: true },
      "/healthz": { target: controlPlane, changeOrigin: true },
    },
    // Poll for changes: a bind-mounted source directory on macOS or Windows
    // does not deliver inotify events into the container, so the default
    // watcher sees nothing and the dev server silently stops reloading.
    watch: { usePolling: true, interval: 300 },
  },
  build: { outDir: "dist", sourcemap: true },
});
