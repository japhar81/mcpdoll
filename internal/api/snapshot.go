// Copyright 2026 Henry Zektser.

package api

import (
	"sort"
	"time"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/registry"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// NewSnapshot renders a snapshot for display.
//
// view may be nil, which is the interesting case: it means the snapshot parsed
// but would not activate. That is reported as a field rather than as an error,
// because an operator looking at a broken snapshot needs to see its contents in
// order to work out what is broken about it.
func NewSnapshot(
	source string,
	signed *snapshotpb.SignedSnapshot,
	snap *snapshotpb.Snapshot,
	view *snapshot.View,
	unservableReason string,
	includeTools bool,
) Snapshot {
	out := Snapshot{
		Source:           source,
		Version:          snap.Version,
		SnapshotID:       snap.Id,
		RegistryDigest:   snap.RegistryDigest,
		Servable:         view != nil,
		UnservableReason: unservableReason,
		Tenants:          []TenantSnapshotSummary{},
	}
	if signed != nil {
		out.KeyID = signed.KeyId
		out.Algorithm = signed.Algorithm
	}
	if snap.BuiltAt != nil {
		built := snap.BuiltAt.AsTime()
		out.BuiltAt = built.Format(time.RFC3339)
		out.Age = time.Since(built).Round(time.Second).String()
	}

	if view != nil {
		// Per tenant, not per audience. A tenant's admitted tools are the pool
		// each of its principals draws from; what any one of them sees is a
		// subset decided by their grants, so a snapshot cannot report a single
		// catalog size (ADR 0016).
		for _, slug := range view.TenantSlugs() {
			tenant := view.Tenant(slug)
			tools := view.ToolsForTenant(tenant.Id)

			tokens := 0
			for _, t := range tools {
				tokens += int(t.Def.TokenEstimate)
			}
			out.Tenants = append(out.Tenants, TenantSnapshotSummary{
				Slug: slug, Name: tenant.Name,
				Tools: len(tools), TokenEstimate: tokens,
			})
		}
	}

	if includeTools {
		servers := map[string]string{}
		for _, s := range snap.Servers {
			servers[s.Id] = s.Name
		}
		out.Tools = []ToolSummary{}
		for _, t := range snap.Tools {
			out.Tools = append(out.Tools, ToolSummary{
				QualifiedName: t.QualifiedName,
				Backend:       servers[t.ServerId],
				EffectClass:   registry.EffectClassName(t.EffectClass),
				Tokens:        int(t.TokenEstimate),
				Digest:        t.Digest,
			})
		}
		sort.Slice(out.Tools, func(i, j int) bool {
			return out.Tools[i].QualifiedName < out.Tools[j].QualifiedName
		})
	}
	return out
}
