import { useQuery } from "@tanstack/react-query";
import { listHooks } from "../lib/api.ts";
import { Screen, Table } from "../components/Screen.tsx";

/** What each hook is for. Kept beside the list because "on_catalog" alone does
 *  not tell a plugin author whether their idea belongs there. */
const PURPOSE: Record<string, string> = {
  on_request: "Before anything is resolved. Rate limits, request-level denial.",
  on_catalog:
    "Shaping the tool list an identity receives. Entitlement filtering.",
  on_tool_call:
    "Before a call reaches the backend. Argument checks, confirmation.",
  on_tool_result: "After the backend answers. Redaction, injection defence.",
  on_error: "A backend or pipeline failure, before the client sees it.",
  on_response: "The final envelope, whatever produced it.",
  on_audit: "The record. Cannot change the outcome — it has already happened.",
};

export function HooksScreen() {
  const q = useQuery({ queryKey: ["hooks"], queryFn: listHooks });

  return (
    <Screen title="Pipeline hooks" isLoading={q.isLoading} error={q.error}>
      <p className="muted">
        The set is closed at seven. Every hook is a place plugin authors must
        reason about and a place the request budget has to be divided, so an
        eighth requires an ADR rather than a pull request.
      </p>
      <Table
        columns={["#", "Hook", "Runs"]}
        rows={(q.data?.hooks ?? []).map((h, i) => [
          <span className="mono">{i + 1}</span>,
          <code>{h}</code>,
          <span className="muted">{PURPOSE[h] ?? ""}</span>,
        ])}
      />
    </Screen>
  );
}
