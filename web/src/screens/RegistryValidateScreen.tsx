import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { validateRegistry } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats } from "../components/Screen.tsx";

/**
 * Validates a pasted document — not the one the server is running.
 *
 * That is the whole point: this is the check you run against a change before
 * merging it, which means it has to work on a document that exists nowhere yet.
 */
export function RegistryValidateScreen() {
  const [content, setContent] = useState("");
  const m = useMutation({ mutationFn: () => validateRegistry(content) });

  return (
    <Screen
      title="Validate a registry document"
      actions={
        <button
          className="primary"
          disabled={!content.trim() || m.isPending}
          onClick={() => m.mutate()}
        >
          {m.isPending ? "Checking…" : "Validate"}
        </button>
      }
    >
      <p className="muted">
        Structure and internal consistency only — unknown keys, dangling
        references, duplicate prefixes, TTLs that try to widen rather than
        narrow. No backend is contacted, so this belongs in a pull-request
        check.
      </p>

      <textarea
        rows={18}
        style={{ width: "100%" }}
        spellCheck={false}
        placeholder="Paste registry.yaml here"
        value={content}
        onChange={(e) => setContent(e.target.value)}
      />

      {m.error != null && (
        <div style={{ marginTop: 16 }}>
          <ErrorBlock error={m.error} />
        </div>
      )}

      {m.data && (
        <div style={{ marginTop: 16 }}>
          <div className="note">
            <strong>Valid.</strong> Every problem this check can find is absent.
            A build can still fail on something only discovery knows — a backend
            that is unreachable, a tool name that collides across two backends.
          </div>
          <Stats
            items={[
              { k: "Org", v: m.data.org, small: true },
              { k: "Version", v: m.data.version },
              { k: "Namespaces", v: m.data.namespaces },
              { k: "Backends", v: m.data.servers },
              { k: "Toolsets", v: m.data.toolsets },
              { k: "Policies", v: m.data.policies },
              { k: "Plugins", v: m.data.plugins },
            ]}
          />
        </div>
      )}
    </Screen>
  );
}
