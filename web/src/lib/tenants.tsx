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

/** A select for the tenant a key will act in. */
export function TenantPicker({
  value,
  onChange,
  slugs,
}: {
  value: string;
  onChange: (slug: string) => void;
  slugs: string[];
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
      </select>
    </label>
  );
}
