import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getCurrentSnapshot } from "../lib/api.ts";
import { SnapshotView } from "../components/SnapshotView.tsx";
import { Screen } from "../components/Screen.tsx";

export function SnapshotScreen() {
  const [tools, setTools] = useState(false);
  const q = useQuery({
    queryKey: ["snapshot", "current", tools],
    queryFn: () => getCurrentSnapshot(tools),
  });

  return (
    <Screen
      title="Current snapshot"
      actions={
        <label className="inline">
          <input
            type="checkbox"
            checked={tools}
            onChange={(e) => setTools(e.target.checked)}
          />
          list every tool
        </label>
      }
      isLoading={q.isLoading}
      error={q.error}
    >
      {q.data && <SnapshotView snapshot={q.data} />}
    </Screen>
  );
}
