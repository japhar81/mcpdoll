import { useQuery } from "@tanstack/react-query";

import { listTenants } from "./api.ts";

/**
 * The tenants this caller can mint a key in.
 *
 * A key acts in exactly one tenant — an MCP session must resolve to one or tool
 * names collide — while a person may reach several. So minting asks, and the
 * options are the tenants the caller can already see: `listTenants` is filtered
 * server-side to what their grants cover.
 *
 * Registry-only slugs are excluded: nothing can authenticate into a tenant with
 * no record, so a key there would be unusable.
 */
export function useMintableTenants() {
  const q = useQuery({ queryKey: ["tenants"], queryFn: listTenants });
  const slugs = (q.data?.registered ?? [])
    .filter((t) => t.id)
    .map((t) => t.slug);
  return { slugs, isLoading: q.isLoading };
}

/**
 * The value meaning "every tenant this key's grants reach" (ADR 0027).
 *
 * A sentinel rather than a separate checkbox: spanning and picking one tenant
 * are the same choice made two ways, and two controls would let somebody set
 * both and leave the form deciding which wins.
 */
export const ALL_TENANTS = "*";

/** A select for the tenant a key will act in. */
export function TenantPicker({
  value,
  onChange,
  slugs,
  allowSpanning = false,
}: {
  value: string;
  onChange: (slug: string) => void;
  slugs: string[];
  /** Offer the spanning option. Off by default: it takes a global permission,
   *  and a control that is always refused is worse than one that is absent. */
  allowSpanning?: boolean;
}) {
  return (
    <label className="field">
      Tenant this key acts in
      <select value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">Select a tenant…</option>
        {slugs.map((s) => (
          <option key={s} value={s}>
            {s}
          </option>
        ))}
        {allowSpanning && (
          <option value={ALL_TENANTS}>
            Every tenant this key can reach — tool names become
            &lt;tenant&gt;.&lt;prefix&gt;.&lt;tool&gt;
          </option>
        )}
      </select>
      {value === ALL_TENANTS && (
        <span className="muted">
          The catalog covers every tenant the grants reach, so the same tool in
          two tenants appears twice under different names. An ordinary key
          cannot address another tenant at all, which is why this one takes a
          platform-wide permission.
        </span>
      )}
    </label>
  );
}

/** Turns the picker's value into the mint request's tenant fields. */
export function mintTenantBody(value: string) {
  return value === ALL_TENANTS
    ? { spans_tenants: true }
    : { tenant: value };
}
