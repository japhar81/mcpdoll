import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";

import {
  getUser,
  listAPIKeys,
  listGrants,
  mintAPIKey,
  revokeAPIKey,
} from "../lib/api.ts";
import { ErrorBlock, Screen, Table } from "../components/Screen.tsx";
import type { Grant, MintedAPIKey } from "../lib/types.ts";

/**
 * One user's agent credentials: what exists, minting a new one, revoking one.
 *
 * The minted secret is rendered once and never refetched, because it is never
 * stored — only its Argon2id hash is. A screen that could show it again would
 * mean the server kept it.
 */
export function APIKeysScreen() {
  const { userId = "", keyId } = useParams();
  const { pathname } = useLocation();
  const minting = pathname.endsWith("/new");
  const revoking = keyId !== undefined;

  const client = useQueryClient();
  const navigate = useNavigate();

  const user = useQuery({
    queryKey: ["user", userId],
    queryFn: () => getUser(userId),
    enabled: userId !== "",
  });
  const q = useQuery({
    queryKey: ["keys", userId],
    queryFn: () => listAPIKeys(userId),
    enabled: userId !== "",
  });
  const owner = useQuery({
    queryKey: ["grants", userId],
    queryFn: () => listGrants(userId),
    enabled: userId !== "",
  });

  const [name, setName] = useState("");
  const [expires, setExpires] = useState("");
  const [narrow, setNarrow] = useState<Grant[]>([]);
  const [minted, setMinted] = useState<MintedAPIKey | null>(null);

  const mint = useMutation({
    mutationFn: () =>
      mintAPIKey(userId, {
        name: name.trim(),
        grants: narrow.length ? narrow : undefined,
        expires_at: expires ? new Date(expires).toISOString() : undefined,
      }),
    onSuccess: (result) => {
      setMinted(result);
      setName("");
      setExpires("");
      setNarrow([]);
      client.invalidateQueries({ queryKey: ["keys", userId] });
    },
  });

  const revoke = useMutation({
    mutationFn: (id: string) => revokeAPIKey(id),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["keys", userId] });
      navigate(`/users/${userId}/keys`);
    },
  });

  const doomed = q.data?.keys.find((k) => k.id === keyId);

  return (
    <Screen
      title={user.data ? `API keys — ${user.data.email}` : "API keys"}
      isLoading={q.isLoading}
      error={q.error}
      actions={
        <>
          <Link className="link" to={`/users/${userId}`}>
            ← user
          </Link>
          {!minting && (
            <>
              {" · "}
              <Link className="link" to={`/users/${userId}/keys/new`}>
                mint a key
              </Link>
            </>
          )}
        </>
      }
    >
      {minted && (
        <div className="card">
          <div className="card-head">
            <strong>{minted.key.name}</strong>
            <span className="badge badge-write">shown once</span>
          </div>
          <p className="muted">
            Copy this now. It is stored only as an Argon2id hash, so nothing —
            including this console — can show it again. If it is lost, mint
            another key and revoke this one.
          </p>
          <pre className="out">{minted.secret}</pre>
          <div className="row">
            <span className="spacer" />
            <button className="secondary" onClick={() => setMinted(null)}>
              I have copied it
            </button>
          </div>
        </div>
      )}

      {minting && (
        <div className="card">
          <div className="card-head">
            <strong>Mint a key</strong>
          </div>
          <label className="field">
            Name
            <input
              placeholder="support-bot production"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label className="field">
            Expires (optional)
            <input
              type="date"
              value={expires}
              onChange={(e) => setExpires(e.target.value)}
            />
          </label>

          <h2>Narrow it</h2>
          <p className="muted">
            A key can narrow what its owner holds but never widen it: effective
            grants are the intersection, recomputed at every resolution.
            Selecting nothing gives the key everything its owner has — which
            also means it shrinks automatically when they are revoked.
          </p>
          {(owner.data?.grants ?? []).map((g) => {
            const on = narrow.some(
              (n) => n.role === g.role && n.scope === g.scope,
            );
            return (
              <label className="inline" key={`${g.role}@${g.scope}`}>
                <input
                  type="checkbox"
                  checked={on}
                  onChange={(e) =>
                    setNarrow(
                      e.target.checked
                        ? [...narrow, g]
                        : narrow.filter(
                            (n) => n.role !== g.role || n.scope !== g.scope,
                          ),
                    )
                  }
                />
                <code>{g.role}</code> @ <code>{g.scope}</code>
              </label>
            );
          })}
          {(owner.data?.grants.length ?? 0) === 0 && (
            <p className="muted">
              This user holds no grants, so any key minted here resolves to an
              empty catalog. That is a legitimate thing to do — grant something
              first and the same key starts working.
            </p>
          )}

          {mint.error != null && <ErrorBlock error={mint.error} />}
          <div className="row">
            <Link className="link" to={`/users/${userId}/keys`}>
              cancel
            </Link>
            <span className="spacer" />
            <button
              className="primary"
              disabled={!name.trim() || mint.isPending}
              onClick={() => mint.mutate()}
            >
              {mint.isPending ? "Minting…" : "Mint"}
            </button>
          </div>
        </div>
      )}

      {revoking && (
        <div className="card">
          <div className="card-head">
            <strong>Revoke {doomed?.name ?? keyId}?</strong>
            <span className="badge badge-bad">immediate</span>
          </div>
          <p className="muted">
            The credential stops resolving at once — this is the one revocation
            that does not wait for a snapshot. The row stays listed so an
            incident review can still see that the key existed and when it was
            last used.
          </p>
          {revoke.error != null && <ErrorBlock error={revoke.error} />}
          <div className="row">
            <Link className="link" to={`/users/${userId}/keys`}>
              cancel
            </Link>
            <span className="spacer" />
            <button
              className="danger"
              disabled={!doomed || revoke.isPending}
              onClick={() => doomed && revoke.mutate(doomed.id)}
            >
              {revoke.isPending ? "Revoking…" : "Revoke"}
            </button>
          </div>
        </div>
      )}

      <Table
        columns={["Name", "Prefix", "State", "Declared", "Last used", ""]}
        rows={(q.data?.keys ?? []).map((k) => [
          k.name,
          <code>{k.prefix}</code>,
          k.revoked_at ? (
            <span className="badge badge-bad">revoked</span>
          ) : k.active ? (
            <span className="badge badge-ok">active</span>
          ) : (
            <span className="badge badge-write">expired</span>
          ),
          k.declared_grants.length ? (
            <span className="mono">{k.declared_grants.length} grant(s)</span>
          ) : (
            <span className="muted">owner&apos;s, whatever they are</span>
          ),
          k.last_used_at ? (
            k.last_used_at.slice(0, 10)
          ) : (
            <span className="muted">never</span>
          ),
          k.active ? (
            <Link className="link" to={`/users/${userId}/keys/${k.id}/revoke`}>
              revoke
            </Link>
          ) : (
            <span className="muted">—</span>
          ),
        ])}
        empty="No keys. This user cannot be acted as by any agent."
      />
      <p className="muted">
        Revoked keys stay listed. A credential that was in use and is not any
        more is exactly what an incident review needs to see, and removing the
        row would remove the evidence.
      </p>
    </Screen>
  );
}
