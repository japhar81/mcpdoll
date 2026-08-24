// Copyright 2026 The MCPDoll Authors.

package store

import (
	"context"
	"sort"

	"github.com/google/uuid"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// The database's contribution to a snapshot.
//
// Everything the data plane needs to authenticate a credential and decide what
// it may see, in the form a signed snapshot carries it. The data plane never
// reads this package — it reads the artifact this produces, which is what makes
// a control-plane outage invisible to a tool call (ADR 0002, ADR 0018).

// SnapshotState is the tenancy and RBAC a snapshot build reads.
type SnapshotState struct {
	Tenants []*snapshotpb.Tenant
	// Principals are API keys, one each. A user with no key contributes
	// nothing: they cannot reach the data plane, so publishing them would only
	// make the artifact bigger.
	Principals []*snapshotpb.Principal
	Catalog    authz.Catalog
}

// SnapshotState reads everything a build needs, in a handful of queries.
//
// Bulk rather than per-user on purpose. A grant change republishes (ADR 0018),
// so a build happens every time an admin toggles anything — and a publish that
// costs one round trip per person on staff makes the console feel broken for
// exactly the organizations that need it most.
func (s *Store) SnapshotState(ctx context.Context) (SnapshotState, error) {
	var out SnapshotState

	tenants, err := s.ListTenants(ctx)
	if err != nil {
		return SnapshotState{}, err
	}
	usersByID := map[uuid.UUID]User{}
	tenantOfUser := map[uuid.UUID]uuid.UUID{}

	for _, t := range tenants {
		out.Tenants = append(out.Tenants, &snapshotpb.Tenant{
			Id: t.ID.String(), Slug: t.Slug, Name: t.Name, Status: t.Status,
		})
	}

	users, err := s.q.ListAllUsers(ctx)
	if err != nil {
		return SnapshotState{}, wrap(err, "listing users")
	}
	for _, row := range users {
		u := userFrom(row)
		usersByID[u.ID] = u
		tenantOfUser[u.ID] = u.TenantID
	}

	ownerGrants := map[uuid.UUID][]authz.Grant{}
	grantRows, err := s.q.ListAllGrants(ctx)
	if err != nil {
		return SnapshotState{}, wrap(err, "listing grants")
	}
	for _, row := range grantRows {
		ownerGrants[row.UserID] = append(ownerGrants[row.UserID],
			authz.Grant{Role: row.Role, Scope: row.Scope})
	}

	keyGrants := map[uuid.UUID][]authz.Grant{}
	keyGrantRows, err := s.q.ListAllAPIKeyGrants(ctx)
	if err != nil {
		return SnapshotState{}, wrap(err, "listing key grants")
	}
	for _, row := range keyGrantRows {
		keyGrants[row.ApiKeyID] = append(keyGrants[row.ApiKeyID],
			authz.Grant{Role: row.Role, Scope: row.Scope})
	}

	keys, err := s.q.ListActiveAPIKeys(ctx)
	if err != nil {
		return SnapshotState{}, wrap(err, "listing keys")
	}
	for _, row := range keys {
		owner, known := usersByID[row.UserID]
		if !known || owner.Status != "active" {
			// A key whose owner is gone or disabled authenticates nothing.
			// Omitted rather than published-and-denied: the snapshot should not
			// carry a credential the system has already decided against.
			continue
		}

		// The intersection happens here because it has to. At request time the
		// data plane holds no owner to intersect against, so an unintersected
		// key in the artifact would be a key that widened its owner's access —
		// the exact thing ADR 0014 refuses.
		declared := keyGrants[row.ID]
		effective := ownerGrants[owner.ID]
		if len(declared) > 0 {
			effective = authz.Intersect(declared, ownerGrants[owner.ID])
		}

		out.Principals = append(out.Principals, &snapshotpb.Principal{
			Id:              row.ID.String(),
			TenantId:        owner.TenantID.String(),
			Subject:         owner.Email,
			Grants:          grantsToProto(effective),
			KeyPrefix:       row.Prefix,
			KeySecretSha256: row.Hash,
		})
	}

	// Sorted, because a snapshot's bytes are signed and compared. Two builds
	// over identical state must produce identical artifacts, or "did anything
	// change?" stops being answerable by comparing digests.
	sort.Slice(out.Principals, func(i, j int) bool {
		return out.Principals[i].Id < out.Principals[j].Id
	})
	sort.Slice(out.Tenants, func(i, j int) bool {
		return out.Tenants[i].Slug < out.Tenants[j].Slug
	})

	if out.Catalog, err = s.Catalog(ctx); err != nil {
		return SnapshotState{}, err
	}
	return out, nil
}

func grantsToProto(grants []authz.Grant) []*snapshotpb.Grant {
	out := make([]*snapshotpb.Grant, 0, len(grants))
	for _, g := range grants {
		out = append(out, &snapshotpb.Grant{Role: g.Role, Scope: g.Scope})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Role < out[j].Role
	})
	return out
}
