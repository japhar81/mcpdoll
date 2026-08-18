import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The dev server proxies /api to the control plane on :3001, so the console is
// same-origin during development and never needs a CORS grant for the common
// path. The API's allowed_origins list still exists for a deployment that
// serves the console from somewhere else.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": { target: "http://localhost:3001", changeOrigin: true },
      "/healthz": { target: "http://localhost:3001", changeOrigin: true },
    },
  },
  build: { outDir: "dist", sourcemap: true },
});
