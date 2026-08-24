import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";

import { callTool, getCatalog, type CallToolInput } from "../lib/api.ts";
import { ErrorBlock, Screen, Stats } from "../components/Screen.tsx";
import { useInspection } from "../lib/inspection.tsx";
import type { CallResult } from "../lib/types.ts";

/**
 * Calls one tool as one credential, through the whole pipeline.
 *
 * A real call: plugins run, the backend is reached, a destructive tool asks for
 * confirmation. What appears here is what the agent holding this key would have
 * received, not a simulation of it.
 */
export function PlaygroundScreen() {
  const { credential, setCredential } = useInspection();
  const [tool, setTool] = useState("");
  const [args, setArgs] = useState("{}");
  const [argsError, setArgsError] = useState<string | null>(null);

  // A deferral in flight. Held rather than folded into the result so the
  // answering round can send back the exact envelope the gateway issued.
  const [pending, setPending] = useState<CallResult | null>(null);
  const [answers, setAnswers] = useState<Record<string, string>>({});

  const catalog = useQuery({
    queryKey: ["catalog", credential, false],
    queryFn: () => getCatalog(credential),
    enabled: credential.trim() !== "",
    retry: false,
  });

  const call = useMutation({
    mutationFn: (input: CallToolInput) => callTool(tool, input),
    onSuccess: (result) => {
      if (result.needs_input) {
        setPending(result);
        setAnswers(
          Object.fromEntries((result.input_requests ?? []).map((id) => [id, "accept"])),
        );
      } else {
        setPending(null);
      }
    },
  });

  function parseArgs(): Record<string, unknown> | null {
    let parsed: unknown;
    try {
      parsed = JSON.parse(args || "{}");
    } catch (e) {
      setArgsError(e instanceof Error ? e.message : String(e));
      return null;
    }
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      setArgsError("arguments must be a JSON object");
      return null;
    }
    setArgsError(null);
    return parsed as Record<string, unknown>;
  }

  function invoke() {
    const parsed = parseArgs();
    if (!parsed) return;
    setPending(null);
    call.mutate({ credential, arguments: parsed });
  }

  function answer() {
    const parsed = parseArgs();
    if (!parsed || !pending) return;
    call.mutate({
      credential,
      arguments: parsed,
      // Echoed unchanged: the envelope binds this approval to this tool, this
      // principal, this tenant, and these arguments.
      request_state: pending.request_state,
      responses: answers,
    });
  }

  const result = call.data;

  return (
    <Screen
      title="Call a tool"
      actions={
        <Link className="link" to="/gateway/catalog">
          ← catalog
        </Link>
      }
    >
      <div className="card">
        <label className="field">
          API key to act as
          <input
            type="password"
            spellCheck={false}
            placeholder="mcpd.…"
            value={credential}
            onChange={(e) => setCredential(e.target.value)}
          />
        </label>
        <div className="row">
          <label className="field">
            Tool
            <select value={tool} onChange={(e) => setTool(e.target.value)}>
              <option value="">Select a tool…</option>
              {(catalog.data?.tools ?? []).map((t) => (
                <option key={t.name} value={t.name}>
                  {t.name}
                </option>
              ))}
            </select>
          </label>
        </div>
        {catalog.error != null && (
          <p className="muted">
            The catalog could not be loaded for this credential, so the list
            above is empty. You can still type a tool name — but if this
            principal cannot see it, the call will be refused.
          </p>
        )}
        <label className="field">
          Arguments (JSON object)
          <textarea
            rows={5}
            spellCheck={false}
            value={args}
            onChange={(e) => setArgs(e.target.value)}
          />
        </label>
        {argsError && <p className="error">{argsError}</p>}
        <div className="row">
          <span className="spacer" />
          <button
            className="primary"
            disabled={!tool || !credential.trim() || call.isPending}
            onClick={invoke}
          >
            {call.isPending ? "Calling…" : "Call"}
          </button>
        </div>
      </div>

      {call.error != null && <ErrorBlock error={call.error} />}

      {pending && (
        <div className="card">
          <div className="card-head">
            <strong>This call is waiting for a human</strong>
            <span className="badge badge-write">input required</span>
          </div>
          <p className="muted">
            The tool did not fail. It returned <code>input_required</code> and an
            opaque <code>requestState</code>, which is sent back with the
            answers. That envelope is signed — unforgeable, though not secret —
            and binds the approval to this tool, this principal, this tenant,
            and these arguments, so it cannot be replayed against anything else.
          </p>
          {(pending.input_requests ?? []).map((id) => (
            <label className="field" key={id}>
              <code>{id}</code>
              <select
                value={answers[id] ?? "accept"}
                onChange={(e) => setAnswers({ ...answers, [id]: e.target.value })}
              >
                <option value="accept">accept</option>
                <option value="decline">decline</option>
                <option value="cancel">cancel</option>
              </select>
            </label>
          ))}
          <div className="row">
            <span className="spacer" />
            <button className="primary" disabled={call.isPending} onClick={answer}>
              {call.isPending ? "Sending…" : "Send answers"}
            </button>
          </div>
        </div>
      )}

      {result && !result.needs_input && (
        <>
          <Stats
            items={[
              {
                k: "Outcome",
                v: result.is_error ? (
                  <span className="badge badge-bad">error</span>
                ) : (
                  <span className="badge badge-ok">ok</span>
                ),
              },
              { k: "Duration", v: `${result.duration_ms} ms` },
              { k: "Tool", v: <code>{result.tool}</code>, small: true },
            ]}
          />
          <h2>Result</h2>
          <pre className="out">{result.text || "(no text content)"}</pre>

          {result.gateway_detail != null && (
            <>
              <h2>What the gateway did</h2>
              <p className="muted">
                The pipeline&apos;s own annotation — which plugin mutated the
                result, which hook denied it, why a deferral was refused.
              </p>
              <pre className="out">
                {JSON.stringify(result.gateway_detail, null, 2)}
              </pre>
            </>
          )}
        </>
      )}
    </Screen>
  );
}
