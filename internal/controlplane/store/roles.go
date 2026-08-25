// Copyright 2026 Henry Zektser.

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/store/dbgen"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// Role is a named bundle of permissions (ADR 0028).
type Role struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Permissions []authz.Permission `json:"permissions"`
	// Builtin roles cannot be deleted — grants would be left pointing at a role
	// nothing recreates, and the seed would put it back on the next boot. Their
	// permissions are still editable.
	Builtin   bool      `json:"builtin"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListRoles returns the catalog as rows, with their permissions.
func (s *Store) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.q.ListRoles(ctx)
	if err != nil {
		return nil, wrap(err, "listing roles")
	}
	catalog, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Role, 0, len(rows))
	for _, row := range rows {
		out = append(out, Role{
			Name: row.Name, Description: row.Description, Builtin: row.Builtin,
			Permissions: catalog.Permissions(row.Name),
			UpdatedAt:   row.UpdatedAt.Time,
		})
	}
	return out, nil
}

// GetRole reads one.
func (s *Store) GetRole(ctx context.Context, name string) (Role, error) {
	row, err := s.q.GetRole(ctx, name)
	if err != nil {
		return Role{}, wrap(err, "reading role %q", name)
	}
	catalog, err := s.Catalog(ctx)
	if err != nil {
		return Role{}, err
	}
	return Role{
		Name: row.Name, Description: row.Description, Builtin: row.Builtin,
		Permissions: catalog.Permissions(row.Name),
		UpdatedAt:   row.UpdatedAt.Time,
	}, nil
}

// PutRole creates or replaces a role's permissions.
//
// Declarative, like grants: the permission set passed is what the role holds
// afterwards, so removing one is expressing the whole set without it. Deltas
// are how a permission gets left behind on a role somebody thought they had
// narrowed.
func (s *Store) PutRole(
	ctx context.Context, name, description string, permissions []authz.Permission,
) (Role, error) {
	if err := validateRoleName(name); err != nil {
		return Role{}, err
	}
	if unknown := authz.UnknownPermissions(permissions); len(unknown) > 0 {
		return Role{}, fmt.Errorf(
			"%w: no such permission: %v. The set is closed — a permission "+
				"nothing enforces would be a role that appears to grant something "+
				"and does not", ErrInvalid, unknown)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Role{}, wrap(err, "beginning the role update")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	if _, err := q.UpsertRole(ctx, dbgen.UpsertRoleParams{
		Name: name, Description: description, Builtin: false,
	}); err != nil {
		return Role{}, wrap(err, "writing role %q", name)
	}
	// Replace rather than merge, in one transaction with the write: a reader
	// between the delete and the inserts would otherwise see a role that grants
	// nothing, and that reader is the snapshot publisher.
	if err := q.ClearRolePermissions(ctx, name); err != nil {
		return Role{}, wrap(err, "clearing permissions for %q", name)
	}
	for _, p := range permissions {
		if err := q.AddRolePermission(ctx, dbgen.AddRolePermissionParams{
			Role: name, Permission: string(p),
		}); err != nil {
			return Role{}, wrap(err, "adding %s to %q", p, name)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Role{}, wrap(err, "committing the role update")
	}
	return s.GetRole(ctx, name)
}

// DeleteRole removes a role nobody holds.
//
// Refused while anybody holds it, rather than cascading. A grant naming a
// deleted role would silently authorize nothing, and the person who lost access
// is not the person running the command — they find out when their agent stops.
func (s *Store) DeleteRole(ctx context.Context, name string) error {
	row, err := s.q.GetRole(ctx, name)
	if err != nil {
		return wrap(err, "reading role %q", name)
	}
	if row.Builtin {
		return fmt.Errorf(
			"%w: %q is built in and cannot be deleted; it would come back on the "+
				"next boot. Narrow its permissions instead", ErrInvalid, name)
	}

	held, err := s.q.CountGrantsOfRole(ctx, name)
	if err != nil {
		return wrap(err, "counting grants of %q", name)
	}
	keyHeld, err := s.q.CountKeyGrantsOfRole(ctx, name)
	if err != nil {
		return wrap(err, "counting key grants of %q", name)
	}
	if held+keyHeld > 0 {
		return fmt.Errorf(
			"%w: %q is still held by %d grant(s) and %d key(s). Deleting it would "+
				"take access away from people you are not looking at — revoke the "+
				"grants first", ErrInvalid, name, held, keyHeld)
	}

	return wrap(s.q.DeleteRoleRow(ctx, name), "deleting role %q", name)
}

// SeedRoles writes the built-in catalog, leaving edits alone.
func (s *Store) SeedRoles(ctx context.Context) error {
	catalog := authz.DefaultCatalog()
	for _, role := range catalog.Roles() {
		if _, err := s.q.UpsertRole(ctx, dbgen.UpsertRoleParams{
			Name: role, Description: authz.DescribeRole(role), Builtin: true,
		}); err != nil {
			return wrap(err, "seeding role %q", role)
		}
	}
	return s.SeedCatalog(ctx)
}

func validateRoleName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: a role needs a name", ErrInvalid)
	}
	if len(name) > 63 {
		return fmt.Errorf("%w: role name %q is longer than 63 characters", ErrInvalid, name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return fmt.Errorf(
				"%w: role name %q may hold only lowercase letters, digits, "+
					"underscore and dash — it appears in a grant scope string, "+
					"where anything else would not round-trip", ErrInvalid, name)
		}
	}
	return nil
}
