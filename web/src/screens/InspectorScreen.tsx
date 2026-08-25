import { useState } from "react";
import { useMutation } from "@tanstack/react-query";

import { mintAPIKey } from "../lib/api.ts";
import { useAuth } from "../lib/auth.tsx";
import { ErrorBlock, Screen } from "../components/Screen.tsx";
import { TenantPicker, useMintableTenants } from "../lib/tenants.tsx";

/** Where the bundled Inspector is served. */
const INSPECTOR_URL = "http://localhost:6274/";

/**
 * The MCP Inspector, embedded, against this gateway.
 *
 * It is not pre-authenticated and cannot be: one Inspector process serves every
 * console user, so a credential baked into it at launch would be handed to
 * whoever opens the page next. The Inspector takes the target's Authorization
 * header from its own connection pane or from a launch flag, and neither can
 * vary per viewer.
 *
 * So: mint a key carrying the signed-in user's own grants, put it on their
 * clipboard, and have them paste it. `mcpdoll inspector` does the same thing
 * without the paste, because there the process is theirs.
 */
export function InspectorScreen() {
  const auth = useAuth();
  const [copied, setCopied] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const [tenant, setTenant] = useState("");
  const { slugs } = useMintableTenants();

  const mint = useMutation({
    mutationFn: async () => {
      const expires = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      const result = await mintAPIKey(auth.session!.user_id!, {
        tenant,
        name: `inspector ${new Date().toISOString().slice(0, 19)}Z`,
        expires_at: expires,
      });
      // Clipboard first, while the click is still the user's gesture — some
      // browsers refuse a write that happens after an await chain they did not
      // initiate. A failure here is not a failed mint: the key is shown below
      // either way.
      try {
        await navigator.clipboard.writeText(result.secret);
        setCopied(true);
      } catch {
        setCopied(false);
      }
      return result;
    },
    onSuccess: (result) => setSecret(result.secret),
  });

  if (!auth.session?.user_id) {
    return (
      <Screen title="Inspector">
        <p className="muted">
          This mints a credential for you, so it needs a signed-in person. The
          deployment token is not one.
        </p>
      </Screen>
    );
  }

  return (
    <Screen
      title="Inspector"
      actions={
        <a className="link" href={INSPECTOR_URL} target="_blank" rel="noreferrer">
          open in a tab →
        </a>
      }
    >
      <div className="card">
        <div className="card-head">
          <strong>1 — get a credential</strong>
        </div>
        <p className="muted">
          A key carrying your own grants, valid for an hour. What the Inspector
          shows is then what you would be served, not what an operator&apos;s
          shared token would be.
        </p>
        <TenantPicker value={tenant} onChange={setTenant} slugs={slugs} />
        <div className="row">
          <button
            className="primary"
            disabled={mint.isPending || !tenant}
            onClick={() => mint.mutate()}
          >
            {mint.isPending ? "Minting…" : "Mint a key and copy it"}
          </button>
          {secret && copied && (
            <span className="muted">Copied — paste it in step 2.</span>
          )}
        </div>
        {mint.error != null && <ErrorBlock error={mint.error} />}
        {secret && !copied && (
          <>
            <p className="muted">
              The clipboard was not available, so copy it by hand. It is shown
              once — nothing stores it.
            </p>
            <pre className="out">{secret}</pre>
          </>
        )}
      </div>

      <div className="card">
        <div className="card-head">
          <strong>2 — paste it into the Inspector</strong>
        </div>
        <p className="muted">
          Open the <code>mcpdoll</code> server below, then{" "}
          <em>Authentication</em> in its connection pane. Set the header name to{" "}
          <code>Authorization</code> and the value to{" "}
          <code>Bearer &lt;paste&gt;</code>, then Connect. The server URL is
          already filled in.
        </p>
        <p className="muted">
          One Inspector serves everyone here, which is why it cannot be
          pre-authenticated: a credential baked in at launch would be handed to
          whoever opens the page next. For the same reason, a header saved here
          is saved for everyone using this development Inspector until the
          container is recreated — the keys minted above expire in an hour,
          which is what keeps that bounded. From your own machine,{" "}
          <code>mcpdoll inspector</code> skips the paste entirely, because that
          process is yours.
        </p>
      </div>

      <iframe
        className="inspector-frame"
        src={INSPECTOR_URL}
        title="MCP Inspector"
      />
    </Screen>
  );
}
