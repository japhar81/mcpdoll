import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { generateSigningKey } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats } from "../components/Screen.tsx";

export function KeysScreen() {
  const [keyId, setKeyId] = useState("");
  const m = useMutation({ mutationFn: () => generateSigningKey(keyId) });

  return (
    <Screen
      title="Signing keys"
      actions={
        <button
          className="primary"
          disabled={!keyId.trim() || m.isPending}
          onClick={() => m.mutate()}
        >
          {m.isPending ? "Generating…" : "Generate keypair"}
        </button>
      }
    >
      <div className="note warn">
        <strong>
          Whoever holds a signing key can publish configuration to every data-plane
          instance. The private half is written to the control plane's key directory
          and is never returned over this connection.
        </strong>{" "}
        The private half is written to the control plane's key directory and is
        never returned over this connection — not to be awkward, but because an
        HTTP response lands in browser memory, a proxy log, and whatever the
        client does next.
      </div>

      <label className="field" style={{ maxWidth: 320 }}>
        Key id — recorded in every snapshot this key signs, and how a verifier
        selects the right public key during a rotation
        <input
          type="text"
          value={keyId}
          spellCheck={false}
          placeholder="prod-2026-q1"
          onChange={(e) => setKeyId(e.target.value)}
        />
      </label>
      <p className="muted">
        Letters, digits, hyphen, underscore — it becomes a filename.
      </p>

      {m.error != null && <ErrorBlock error={m.error} />}

      {m.data && (
        <>
          <Stats
            items={[
              { k: "Key id", v: m.data.key_id, small: true },
              { k: "Written to", v: m.data.directory, small: true },
            ]}
          />
          <h2>Trust entry</h2>
          <p className="muted">
            Add this to each data plane's <code>trusted_signing_keys</code> before
            signing anything with the new key. A data plane trusts several keys at
            once, so a rotation needs no restart.
          </p>
          <pre className="out">{m.data.trust_entry}</pre>
        </>
      )}
    </Screen>
  );
}
