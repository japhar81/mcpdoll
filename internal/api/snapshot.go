// Copyright 2026 The MCPDoll Authors.

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
		Org:              snap.OrgId,
		RegistryDigest:   snap.RegistryDigest,
		Servable:         view != nil,
		UnservableReason: unservableReason,
		Audiences:        []AudienceSummary{},
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
		for _, slug := range view.AudienceSlugs() {
			av := view.Audience(slug)
			out.Audiences = append(out.Audiences, AudienceSummary{
				Slug:          slug,
				Name:          av.Audience.Name,
				Tools:         len(av.Tools),
				TTLMs:         av.TTLMs,
				CacheScope:    av.CacheScope(),
				TokenEstimate: av.TokenEstimate,
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
