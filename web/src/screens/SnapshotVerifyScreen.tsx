import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { verifySnapshot } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats, Table } from "../components/Screen.tsx";
import { readFileAsBase64 } from "../lib/file.ts";

export function SnapshotVerifyScreen() {
  const [content, setContent] = useState("");
  const [name, setName] = useState("");
  const [keys, setKeys] = useState("");

  const trusted = keys
    .split("\n")
    .map((k) => k.trim())
    .filter(Boolean);

  const m = useMutation({ mutationFn: () => verifySnapshot(content, trusted) });

  return (
    <Screen
      title="Verify a snapshot"
      actions={
        <button
          className="primary"
          disabled={!content || trusted.length === 0 || m.isPending}
          onClick={() => m.mutate()}
        >
          {m.isPending ? "Verifying…" : "Verify"}
        </button>
      }
    >
      <p className="muted">
        Two checks: the signature over exactly the transmitted bytes, then whether
        the snapshot would activate. A correctly signed snapshot with a dangling
        reference is refused by every data plane.
      </p>

      <div className="row">
        <label className="field" style={{ maxWidth: 380 }}>
          Snapshot file (.pb)
          <input
            type="file"
            onChange={async (e) => {
              const file = e.target.files?.[0];
              if (!file) return;
              setName(file.name);
              setContent(await readFileAsBase64(file));
            }}
          />
        </label>
      </div>
      {name && <p className="muted">{name}</p>}

      <label className="field" style={{ marginTop: 12 }}>
        Trusted keys — one <code>keyID:base64PublicKey</code> per line
        <textarea
          rows={4}
          spellCheck={false}
          value={keys}
          onChange={(e) => setKeys(e.target.value)}
          placeholder="dev:AAAA…"
        />
      </label>
      <p className="muted">
        You supply these rather than the server using its own, so this answers
        whether that particular data plane would accept it.
      <em>that</em> data plane accept it?” — including one whose trust
        list you are about to change.
      </p>

      {m.error != null && (
        <div style={{ marginTop: 16 }}>
          <ErrorBlock error={m.error} />
        </div>
      )}

      {m.data && (
        <div style={{ marginTop: 16 }}>
          <div className="note">
            <strong>Signature valid, and it would activate.</strong>
          </div>
          <Stats
            items={[
              { k: "Version", v: m.data.version },
              { k: "Signed by", v: m.data.key_id, small: true },
              { k: "Tools", v: m.data.tools },
              { k: "Tenants", v: m.data.tenants.length },
            ]}
          />
          <Table
            columns={["Tenant"]}
            rows={m.data.tenants.map((t) => [<code>{t}</code>])}
            empty="This snapshot names no tenants, so it would serve nobody."
          />
        </div>
      )}
    </Screen>
  );
}
