import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { inspectSnapshot } from "../lib/api.ts";
import { SnapshotView } from "../components/SnapshotView.tsx";
import { ErrorBlock, Screen } from "../components/Screen.tsx";
import { readFileAsBase64 } from "../lib/file.ts";

/**
 * Inspects an uploaded snapshot without verifying it.
 *
 * Deliberately: a snapshot signed by a key you do not hold is exactly the one
 * you most need to look at, and refusing to display it turns a diagnosable
 * situation into a blank screen. The key id is shown so the signature question
 * can be answered next door, on the verify screen.
 */
export function SnapshotInspectScreen() {
  const [content, setContent] = useState("");
  const [name, setName] = useState("");
  const [tools, setTools] = useState(false);
  const m = useMutation({ mutationFn: () => inspectSnapshot(content, tools) });

  return (
    <Screen
      title="Inspect a snapshot"
      actions={
        <>
          <label className="inline">
            <input
              type="checkbox"
              checked={tools}
              onChange={(e) => setTools(e.target.checked)}
            />
            list every tool
          </label>
          <button
            className="primary"
            disabled={!content || m.isPending}
            onClick={() => m.mutate()}
          >
            {m.isPending ? "Reading…" : "Inspect"}
          </button>
        </>
      }
    >
      <p className="muted">
        The file's bytes are sent, not its path — a server that opens whatever
        path a caller names is an arbitrary file read. The signature is{" "}
        <em>not</em> checked here; use Verify for that.
      </p>

      <label className="field" style={{ maxWidth: 420 }}>
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
      {name && <p className="muted">{name}</p>}

      {m.error != null && (
        <div style={{ marginTop: 16 }}>
          <ErrorBlock error={m.error} />
        </div>
      )}
      {m.data && (
        <div style={{ marginTop: 16 }}>
          <SnapshotView snapshot={m.data} />
        </div>
      )}
    </Screen>
  );
}
