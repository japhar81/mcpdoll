import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // Node, not jsdom: what is worth testing here is the client's error
    // handling and the pure helpers. Rendering React into a fake DOM to assert
    // a table has rows tests the framework, not this code — the browser pass
    // covers what a DOM test would.
    environment: "node",
    include: ["src/**/*.test.ts"],
  },
});
