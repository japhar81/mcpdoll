import { useIdentity } from "../lib/identity.tsx";

/**
 * Subject and groups inputs, bound to the shared inspection identity.
 *
 * Presenting an identity is not the same as authenticating as one — the data
 * plane decides whether the operator may inspect on someone's behalf. These
 * fields say what to claim, not permission to claim it.
 */
export function IdentityFields() {
  const { identity, setIdentity } = useIdentity();
  return (
    <div className="row">
      <label className="field">
        Subject
        <input
          type="text"
          spellCheck={false}
          placeholder="alice@example.com"
          value={identity.subject}
          onChange={(e) =>
            setIdentity({ ...identity, subject: e.target.value })
          }
        />
      </label>
      <label className="field">
        Groups — comma separated
        <input
          type="text"
          spellCheck={false}
          placeholder="eng-platform"
          value={identity.groups}
          onChange={(e) => setIdentity({ ...identity, groups: e.target.value })}
        />
      </label>
    </div>
  );
}
