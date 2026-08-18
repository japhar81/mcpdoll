import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import {
  callTool,
  getAudienceCatalog,
  type CallToolInput,
} from "../lib/api.ts";
import { ErrorBlock, Screen, Stats } from "../components/Screen.tsx";
import { IdentityFields } from "../components/IdentityFields.tsx";
import { toGroupList, useIdentity } from "../lib/identity.tsx";
import type { CallResult } from "../lib/types.ts";

/**
 * Calls one tool as one identity, through the whole pipeline.
 *
 * The point is that this is a real call: plugins run, the backend is reached,
 * a destructive tool asks for confirmation. What appears here is what an agent
 * would have received, not a simulation of it.
 */
export function PlaygroundScreen() {
  const { slug = "" } = useParams();
  const { identity } = useIdentity();
  const [tool, setTool] = useState("");
  const [args, setArgs] = useState("{}");
  const [argsError, setArgsError] = useState<string | null>(null);

  // A deferral in flight. Held rather than folded into the result so the
  // answering round can send back the exact envelope the gateway issued.
  const [pending, setPending] = useState<CallResult | null>(null);
  const [answers, setAnswers] = useState<Record<string, string>>({});

  const catalog = useQuery({
    queryKey: ["catalog", slug, identity.subject, identity.groups, false],
    queryFn: () =>
      getAudienceCatalog(slug, {
        subject: identity.subject,
        groups: toGroupList(identity.groups),
      }),
    retry: false,
  });

  const call = useMutation({
    mutationFn: (input: CallToolInput) => callTool(slug, tool, input),
    onSuccess: (result) => {
      if (result.needs_input) {
        setPending(result);
        setAnswers(
          Object.fromEntries(
            (result.input_requests ?? []).map((id) => [id, "accept"]),
          ),
        );
      } else {
        setPending(null);
      }
    },
  });

  function invoke() {
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(args || "{}") as Record<string, unknown>;
    } catch (e) {
      setArgsError(e instanceof Error ? e.message : String(e));
      return;
    }
    if (
      parsed === null ||
      typeof parsed !== "object" ||
      Array.isArray(parsed)
    ) {
      setArgsError("arguments must be a JSON object");
      return;
    }
    setArgsError(null);
    setPending(null);
    call.mutate({
      subject: identity.subject,
      groups: toGroupList(identity.groups),
      arguments: parsed,
    });
  }

  function answer() {
    if (!pending) return;
    call.mutate({
      subject: identity.subject,
      groups: toGroupList(identity.groups),
      arguments: JSON.parse(args || "{}") as Record<string, unknown>,
      // Echoed unchanged: the envelope binds this approval to this tool, this
      // principal, and these arguments.
      request_state: pending.request_state,
      responses: answers,
    });
  }

  const result = call.data;

  return (
    <Screen
      title={`Call a tool — ${slug}`}
      actions={
        <Link className="link" to={`/gateway/audiences/${slug}/catalog`}>
          ← catalog
        </Link>
      }
    >
      <div className="card">
        <IdentityFields />
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
            The catalog could not be loaded for this identity, so the list above
            is empty. You can still type a tool name — but if this identity
            cannot see it, the call will be refused.
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
            disabled={!tool || call.isPending}
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
            The tool did not fail. It returned <code>input_required</code> and
            an opaque <code>requestState</code>, which is sent back with the
            answers. That envelope is signed — unforgeable, though not secret —
            and binds the approval to this tool, this principal, and these
            arguments, so it cannot be replayed against anything else.
          </p>
          {(pending.input_requests ?? []).map((id) => (
            <label className="field" key={id}>
              <code>{id}</code>
              <select
                value={answers[id] ?? "accept"}
                onChange={(e) =>
                  setAnswers({ ...answers, [id]: e.target.value })
                }
              >
                <option value="accept">accept</option>
                <option value="decline">decline</option>
                <option value="cancel">cancel</option>
              </select>
            </label>
          ))}
          <div className="row">
            <span className="spacer" />
            <button
              className="primary"
              disabled={call.isPending}
              onClick={answer}
            >
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
                The pipeline's own annotation — which plugin mutated the result,
                which hook denied it, why a deferral was refused.
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
