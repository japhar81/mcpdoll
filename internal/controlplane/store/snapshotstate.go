// Copyright 2026 Henry Zektser.

package store

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// The database's contribution to a snapshot.
//
// Everything the data plane needs to authenticate a credential and decide what
// it may see, in the form a signed snapshot carries it. The data plane never
// reads this package — it reads the artifact this produces, which is what makes
// a control-plane outage invisible to a tool call (ADR 0002, ADR 0018).

// SnapshotState is the tenancy a snapshot build reads.
//
// Tenants only. Principals moved to their own artifact (ADR 0024) — but tenants
// stay, because a backend binding names one and a tool is admitted per tenant,
// so the snapshot genuinely cannot be built without them.
type SnapshotState struct {
	Tenants []*snapshotpb.Tenant
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
	for _, t := range tenants {
		out.Tenants = append(out.Tenants, &snapshotpb.Tenant{
			Id: t.ID.String(), Slug: t.Slug, Name: t.Name, Status: t.Status,
		})
	}

	// Sorted, because the snapshot is signed and compared: two builds over
	// identical state must produce identical bytes, or "did anything change?"
	// stops being answerable by comparing digests.
	sort.Slice(out.Tenants, func(i, j int) bool {
		return out.Tenants[i].Slug < out.Tenants[j].Slug
	})
	return out, nil
}

// PrincipalSetState is who exists and what they hold.
//
// Read on its own, because it is published on its own: minting a key, issuing a
// grant, and disabling a user all change this and none of them should cost a
// discovery sweep of every backend (ADR 0024).
func (s *Store) PrincipalSetState(ctx context.Context) (*snapshotpb.PrincipalSet, error) {
	tenants, err := s.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	knownTenant := map[uuid.UUID]bool{}
	for _, t := range tenants {
		knownTenant[t.ID] = true
	}

	usersByID := map[uuid.UUID]User{}
	principals := []*snapshotpb.Principal{}
	users, err := s.q.ListAllUsers(ctx)
	if err != nil {
		return nil, wrap(err, "listing users")
	}
	for _, row := range users {
		u := userFrom(row)
		usersByID[u.ID] = u
	}

	ownerGrants := map[uuid.UUID][]authz.Grant{}
	grantRows, err := s.q.ListAllGrants(ctx)
	if err != nil {
		return nil, wrap(err, "listing grants")
	}
	for _, row := range grantRows {
		ownerGrants[row.UserID] = append(ownerGrants[row.UserID],
			authz.Grant{Role: row.Role, Scope: row.Scope})
	}

	keyGrants := map[uuid.UUID][]authz.Grant{}
	keyGrantRows, err := s.q.ListAllAPIKeyGrants(ctx)
	if err != nil {
		return nil, wrap(err, "listing key grants")
	}
	for _, row := range keyGrantRows {
		keyGrants[row.ApiKeyID] = append(keyGrants[row.ApiKeyID],
			authz.Grant{Role: row.Role, Scope: row.Scope})
	}

	keys, err := s.q.ListActiveAPIKeys(ctx)
	if err != nil {
		return nil, wrap(err, "listing keys")
	}
	for _, row := range keys {
		owner, known := usersByID[row.UserID]
		if !known || owner.Status != "active" || !knownTenant[row.TenantID] {
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

		principals = append(principals, &snapshotpb.Principal{
			Id: row.ID.String(),
			// The key's tenant. An MCP session resolves to exactly one, and
			// this is where that comes from.
			TenantId:        row.TenantID.String(),
			Subject:         owner.Email,
			Grants:          grantsToProto(effective),
			KeyPrefix:       row.Prefix,
			KeySecretSha256: row.Hash,
		})
	}

	sort.Slice(principals, func(i, j int) bool { return principals[i].Id < principals[j].Id })

	catalog, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	rolePermissions := make([]*snapshotpb.RolePermission, 0, len(catalog))
	// Sorted, because the set is signed and a map's iteration order would make
	// two reads of identical state produce different bytes.
	for _, role := range catalog.Roles() {
		for _, permission := range catalog.Permissions(role) {
			rolePermissions = append(rolePermissions, &snapshotpb.RolePermission{
				Role: role, Permission: string(permission),
			})
		}
	}

	return &snapshotpb.PrincipalSet{
		RolePermissions: rolePermissions,
		Principals:      principals,
	}, nil
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

// PublishPrincipalSet bumps the version and returns the set to sign.
//
// The bump and the read happen together so the version always describes the
// bytes: reading first and bumping after would publish a set stamped with a
// version that predates it, and a data plane comparing versions would refuse
// the newer content.
func (s *Store) PublishPrincipalSet(ctx context.Context) (*snapshotpb.PrincipalSet, error) {
	row, err := s.q.BumpPrincipalVersion(ctx)
	if err != nil {
		return nil, wrap(err, "bumping the principal version")
	}
	set, err := s.PrincipalSetState(ctx)
	if err != nil {
		return nil, err
	}
	set.Version = row.PrincipalVersion
	if set.Version <= 0 {
		// Version zero would be refused as not newer than a data plane's
		// starting state, so a fresh install publishes 1 with whatever it has.
		set.Version = 1
	}
	return set, nil
}

// PrincipalVersion is the version last published.
func (s *Store) PrincipalVersion(ctx context.Context) (int64, error) {
	row, err := s.q.GetRevocationState(ctx)
	if err != nil {
		return 0, wrap(err, "reading the principal version")
	}
	return row.PrincipalVersion, nil
}
