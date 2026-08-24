// Copyright 2026 Henry Zektser.

// Package store is the control plane's durable state: tenants, users,
// credentials, and RBAC grants.
//
// It sits above sqlc-generated code in `dbgen` and exists to do the part a
// generator cannot: turn rows into the domain types the rest of the system
// already speaks, and enforce the invariants that are properties of the model
// rather than of the schema.
//
// The data plane never touches this package. Grants reach it inside a signed
// snapshot (ADR 0018), which is what preserves the property that a
// control-plane outage cannot stop a tool call.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/store/dbgen"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// Sentinel errors. Callers map these to exit codes and HTTP statuses, so they
// are part of this package's contract rather than incidental.
var (
	// ErrNotFound is a row that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is a uniqueness violation — a slug or email already taken.
	ErrConflict = errors.New("already exists")
	// ErrInvalid is input that broke a rule this package enforces.
	ErrInvalid = errors.New("invalid")
)

// Store is the control plane's database.
type Store struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

// Open connects and verifies the connection before returning.
//
// Verifying at open rather than on first use means a bad DSN stops the process
// at startup with a legible message, instead of surfacing as a failed request
// minutes later.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: parsing the database URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connecting: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: the database did not answer: %w", err)
	}
	return &Store{pool: pool, q: dbgen.New(pool)}, nil
}

// Migrate applies any pending schema migrations.
func (s *Store) Migrate(ctx context.Context) error { return Migrate(ctx, s.pool) }

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the connection pool for migrations and health checks.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ------------------------------------------------------------- domain types --

// Tenant is an isolation boundary.
type Tenant struct {
	ID     uuid.UUID `json:"id"`
	Slug   string    `json:"slug"`
	Name   string    `json:"name"`
	Status string    `json:"status"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// User is a person or service identity inside a tenant.
type User struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name,omitempty"`
	Status      string    `json:"status"`
	// HasPassword rather than the hash. A password hash has no business
	// leaving this package, and every caller only ever needs to know whether
	// local sign-in is possible.
	HasPassword bool      `json:"has_password"`
	CreatedAt   time.Time `json:"created_at"`
}

// APIKey is an agent credential. Never carries the secret.
type APIKey struct {
	ID       uuid.UUID     `json:"id"`
	UserID   uuid.UUID     `json:"user_id"`
	Name     string        `json:"name"`
	Prefix   string        `json:"prefix"`
	Declared []authz.Grant `json:"declared_grants"`

	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether a key may still authenticate.
func (k APIKey) Active(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
		return false
	}
	return true
}

// ------------------------------------------------------------------ tenants --

// CreateTenant adds a tenant.
func (s *Store) CreateTenant(ctx context.Context, slug, name string) (Tenant, error) {
	if err := validateSlug(slug); err != nil {
		return Tenant{}, err
	}
	if name == "" {
		return Tenant{}, fmt.Errorf("%w: a tenant needs a name", ErrInvalid)
	}

	row, err := s.q.CreateTenant(ctx, dbgen.CreateTenantParams{
		Slug: slug, Name: name, Metadata: []byte(`{}`),
	})
	if err != nil {
		return Tenant{}, wrap(err, "creating tenant %q", slug)
	}
	return tenantFrom(row), nil
}

// GetTenantBySlug reads a tenant by the slug that appears in scope strings.
func (s *Store) GetTenantBySlug(ctx context.Context, slug string) (Tenant, error) {
	row, err := s.q.GetTenantBySlug(ctx, slug)
	if err != nil {
		return Tenant{}, wrap(err, "reading tenant %q", slug)
	}
	return tenantFrom(row), nil
}

// GetTenant reads a tenant by id.
func (s *Store) GetTenant(ctx context.Context, id uuid.UUID) (Tenant, error) {
	row, err := s.q.GetTenant(ctx, id)
	if err != nil {
		return Tenant{}, wrap(err, "reading tenant %s", id)
	}
	return tenantFrom(row), nil
}

// ListTenants returns every tenant, ordered by slug.
func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.q.ListTenants(ctx)
	if err != nil {
		return nil, wrap(err, "listing tenants")
	}
	out := make([]Tenant, 0, len(rows))
	for _, row := range rows {
		out = append(out, tenantFrom(row))
	}
	return out, nil
}

// -------------------------------------------------------------------- users --

// CreateUser adds a user to a tenant, optionally with a local password.
func (s *Store) CreateUser(ctx context.Context, tenantID uuid.UUID, email, displayName, password string) (User, error) {
	if email == "" {
		return User{}, fmt.Errorf("%w: a user needs an email", ErrInvalid)
	}

	var hash *string
	if password != "" {
		encoded, err := HashSecret(password)
		if err != nil {
			return User{}, err
		}
		hash = &encoded
	}

	row, err := s.q.CreateUser(ctx, dbgen.CreateUserParams{
		TenantID: tenantID, Email: email,
		DisplayName: nilIfEmpty(displayName), PasswordHash: hash,
	})
	if err != nil {
		return User{}, wrap(err, "creating user %q", email)
	}
	return userFrom(row), nil
}

// GetUser reads one user.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	row, err := s.q.GetUser(ctx, id)
	if err != nil {
		return User{}, wrap(err, "reading user %s", id)
	}
	return userFrom(row), nil
}

// ListUsersByTenant returns a tenant's users.
func (s *Store) ListUsersByTenant(ctx context.Context, tenantID uuid.UUID) ([]User, error) {
	rows, err := s.q.ListUsersByTenant(ctx, tenantID)
	if err != nil {
		return nil, wrap(err, "listing users")
	}
	out := make([]User, 0, len(rows))
	for _, row := range rows {
		out = append(out, userFrom(row))
	}
	return out, nil
}

// CountUsersByTenant returns user counts keyed by tenant id.
//
// One query for the whole list. The alternative — asking per tenant — is the
// shape that makes a tenant list slower every time somebody onboards.
func (s *Store) CountUsersByTenant(ctx context.Context) (map[uuid.UUID]int, error) {
	rows, err := s.q.CountUsersByTenant(ctx)
	if err != nil {
		return nil, wrap(err, "counting users")
	}
	out := make(map[uuid.UUID]int, len(rows))
	for _, row := range rows {
		out[row.TenantID] = int(row.Users)
	}
	return out, nil
}

// UpdateUser changes a user's display name and status.
//
// Status is validated here rather than trusted: "disabled" is the offboarding
// path, and a typo that silently stored "disable" would leave an account the
// operator believes is shut off still authenticating.
func (s *Store) UpdateUser(ctx context.Context, id uuid.UUID, displayName, status string) (User, error) {
	switch status {
	case "active", "disabled":
	default:
		return User{}, fmt.Errorf("%w: status %q is not active or disabled", ErrInvalid, status)
	}
	row, err := s.q.UpdateUser(ctx, dbgen.UpdateUserParams{
		ID: id, DisplayName: nilIfEmpty(displayName), Status: status,
	})
	if err != nil {
		return User{}, wrap(err, "updating user %s", id)
	}
	return userFrom(row), nil
}

// SetGrants makes a user's grants exactly the given set.
//
// Declarative rather than add/remove, because the question an operator is
// answering is "what should this person hold", and expressing that as a
// sequence of deltas is how a revocation gets forgotten. Grants already held
// are left alone, so re-issuing an unchanged set does not churn the audit
// trail.
func (s *Store) SetGrants(ctx context.Context, userID uuid.UUID, want []authz.Grant, grantedBy *uuid.UUID) error {
	for _, g := range want {
		if err := g.Validate(); err != nil {
			return fmt.Errorf("%w: %s", ErrInvalid, err)
		}
	}

	have, err := s.GrantsForUser(ctx, userID)
	if err != nil {
		return err
	}

	wanted := make(map[authz.Grant]bool, len(want))
	for _, g := range want {
		wanted[g] = true
	}
	held := make(map[authz.Grant]bool, len(have))
	for _, g := range have {
		held[g] = true
	}

	for g := range wanted {
		if !held[g] {
			if err := s.Grant(ctx, userID, g, grantedBy); err != nil {
				return err
			}
		}
	}
	for g := range held {
		if !wanted[g] {
			if err := s.RevokeGrant(ctx, userID, g); err != nil {
				return err
			}
		}
	}
	return nil
}

// VerifyPassword checks a local credential.
//
// A user with no password hash is still run through a verification, so that
// "this account has no local password" costs the same as "wrong password".
// Returning early would make the difference measurable.
func (s *Store) VerifyPassword(ctx context.Context, tenantID uuid.UUID, email, password string) (User, error) {
	row, err := s.q.GetUserByEmail(ctx, dbgen.GetUserByEmailParams{TenantID: tenantID, Email: email})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			VerifySecret(password, "")
			return User{}, ErrNotFound
		}
		return User{}, wrap(err, "reading user %q", email)
	}

	stored := ""
	if row.PasswordHash != nil {
		stored = *row.PasswordHash
	}
	if !VerifySecret(password, stored) {
		return User{}, ErrNotFound
	}
	if row.Status != "active" {
		return User{}, fmt.Errorf("%w: the account is disabled", ErrInvalid)
	}
	return userFrom(row), nil
}

// --------------------------------------------------------------------- rbac --

// Catalog reads the role→permission catalog.
//
// Falls back to the built-in defaults when the table is empty, so a fresh
// install authorizes correctly before anyone has seeded it. An empty catalog
// would otherwise be default-deny for everyone, including the administrator
// who needs to fix it.
func (s *Store) Catalog(ctx context.Context) (authz.Catalog, error) {
	rows, err := s.q.ListRolePermissions(ctx)
	if err != nil {
		return nil, wrap(err, "reading the role catalog")
	}
	if len(rows) == 0 {
		return authz.DefaultCatalog(), nil
	}

	catalog := authz.Catalog{}
	for _, row := range rows {
		if catalog[row.Role] == nil {
			catalog[row.Role] = map[authz.Permission]struct{}{}
		}
		catalog[row.Role][authz.Permission(row.Permission)] = struct{}{}
	}
	return catalog, nil
}

// SeedCatalog writes the built-in defaults, leaving existing rows alone.
func (s *Store) SeedCatalog(ctx context.Context) error {
	catalog := authz.DefaultCatalog()
	for _, role := range catalog.Roles() {
		for _, permission := range catalog.Permissions(role) {
			if err := s.q.AddRolePermission(ctx, dbgen.AddRolePermissionParams{
				Role: role, Permission: string(permission),
			}); err != nil {
				return wrap(err, "seeding role %q", role)
			}
		}
	}
	return nil
}

// Grant gives a user a role within a scope.
func (s *Store) Grant(ctx context.Context, userID uuid.UUID, grant authz.Grant, grantedBy *uuid.UUID) error {
	if err := grant.Validate(); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	_, err := s.q.CreateGrant(ctx, dbgen.CreateGrantParams{
		UserID: userID, Role: grant.Role, Scope: grant.Scope, GrantedBy: grantedBy,
	})
	return wrap(err, "granting %s @ %s", grant.Role, grant.Scope)
}

// RevokeGrant removes a grant. Removing one that does not exist is not an
// error: the caller's intent is that the user should not hold it, and they do
// not.
//
// Named for what it removes, because [Store.Revoke] now means something else
// entirely — refusing a credential outright (ADR 0023) — and two methods called
// `Revoke` on one type would be a genuinely dangerous ambiguity.
func (s *Store) RevokeGrant(ctx context.Context, userID uuid.UUID, grant authz.Grant) error {
	return wrap(s.q.RevokeGrant(ctx, dbgen.RevokeGrantParams{
		UserID: userID, Role: grant.Role, Scope: grant.Scope,
	}), "revoking %s @ %s", grant.Role, grant.Scope)
}

// GrantsForUser reads a user's grants.
func (s *Store) GrantsForUser(ctx context.Context, userID uuid.UUID) ([]authz.Grant, error) {
	rows, err := s.q.ListGrantsByUser(ctx, userID)
	if err != nil {
		return nil, wrap(err, "reading grants")
	}
	out := make([]authz.Grant, 0, len(rows))
	for _, row := range rows {
		out = append(out, authz.Grant{Role: row.Role, Scope: row.Scope})
	}
	return out, nil
}

// ----------------------------------------------------------------- api keys --

// MintAPIKey creates a key and returns its plaintext exactly once.
//
// The declared grants are stored as given, without checking them against the
// owner's. That is deliberate and is ADR 0014's reasoning: an owner's grants
// can shrink after a key is minted, so the check has to happen at resolution
// anyway — and doing it here as well would only make the *first* moment strict
// while leaving every later one to the real check.
func (s *Store) MintAPIKey(
	ctx context.Context,
	userID uuid.UUID,
	name string,
	declared []authz.Grant,
	expiresAt *time.Time,
) (key APIKey, plaintext string, err error) {
	if name == "" {
		return APIKey{}, "", fmt.Errorf("%w: a key needs a name", ErrInvalid)
	}
	for _, g := range declared {
		if err := g.Validate(); err != nil {
			return APIKey{}, "", fmt.Errorf("%w: %s", ErrInvalid, err)
		}
	}

	plaintext, prefix, hash, err := NewAPIKey()
	if err != nil {
		return APIKey{}, "", err
	}

	row, err := s.q.CreateAPIKey(ctx, dbgen.CreateAPIKeyParams{
		UserID: userID, Name: name, Prefix: prefix, Hash: hash,
		ExpiresAt: timestamptzPtr(expiresAt),
	})
	if err != nil {
		return APIKey{}, "", wrap(err, "creating key %q", name)
	}

	for _, g := range declared {
		if _, err := s.q.AddAPIKeyGrant(ctx, dbgen.AddAPIKeyGrantParams{
			ApiKeyID: row.ID, Role: g.Role, Scope: g.Scope,
		}); err != nil {
			return APIKey{}, "", wrap(err, "attaching grant %s @ %s", g.Role, g.Scope)
		}
	}

	out := apiKeyFrom(row)
	out.Declared = declared
	return out, plaintext, nil
}

// Resolved is who a credential turns out to be, with the grants it may use.
type Resolved struct {
	User   User
	Tenant Tenant
	Key    *APIKey
	// Grants are already intersected for a key (ADR 0014), so a caller never
	// has to remember to do it. There is exactly one way to get effective
	// grants out of this package, and it is this field.
	Grants []authz.Grant
}

// ResolveAPIKey authenticates a presented key and returns its principal.
//
// The intersection with the owner's grants happens here, on every call. That is
// what makes revoking a user revoke every key they hold.
func (s *Store) ResolveAPIKey(ctx context.Context, presented string) (Resolved, error) {
	prefix, secret, err := SplitAPIKey(presented)
	if err != nil {
		return Resolved{}, ErrNotFound
	}

	row, err := s.q.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Still hash, so a wrong prefix and a wrong secret are
			// indistinguishable in timing. Cheap now that this is SHA-256
			// (ADR 0021), and still worth doing: the asymmetry is what an
			// attacker probes for.
			VerifyKeySecret(secret, "")
			return Resolved{}, ErrNotFound
		}
		return Resolved{}, wrap(err, "reading the key")
	}

	if !VerifyKeySecret(secret, row.Hash) {
		return Resolved{}, ErrNotFound
	}

	key := apiKeyFrom(row)
	if !key.Active(time.Now()) {
		return Resolved{}, fmt.Errorf("%w: the key is revoked or expired", ErrInvalid)
	}

	user, err := s.GetUser(ctx, key.UserID)
	if err != nil {
		return Resolved{}, err
	}
	if user.Status != "active" {
		return Resolved{}, fmt.Errorf("%w: the account is disabled", ErrInvalid)
	}

	tenantRow, err := s.q.GetTenant(ctx, user.TenantID)
	if err != nil {
		return Resolved{}, wrap(err, "reading the tenant")
	}
	tenant := tenantFrom(tenantRow)
	if tenant.Status != "active" {
		return Resolved{}, fmt.Errorf("%w: the tenant is %s", ErrInvalid, tenant.Status)
	}

	ownerGrants, err := s.GrantsForUser(ctx, user.ID)
	if err != nil {
		return Resolved{}, err
	}

	declaredRows, err := s.q.ListAPIKeyGrants(ctx, key.ID)
	if err != nil {
		return Resolved{}, wrap(err, "reading key grants")
	}
	declared := make([]authz.Grant, 0, len(declaredRows))
	for _, row := range declaredRows {
		declared = append(declared, authz.Grant{Role: row.Role, Scope: row.Scope})
	}
	key.Declared = declared

	// A key that declares nothing carries nothing. It does not inherit its
	// owner's grants by omission — that would be the widening ADR 0014 refuses.
	effective := authz.Intersect(declared, ownerGrants)

	return Resolved{User: user, Tenant: tenant, Key: &key, Grants: effective}, nil
}

// GetAPIKey reads one key by id, without its secret.
func (s *Store) GetAPIKey(ctx context.Context, id uuid.UUID) (APIKey, error) {
	row, err := s.q.GetAPIKey(ctx, id)
	if err != nil {
		return APIKey{}, wrap(err, "reading key %s", id)
	}
	return apiKeyFrom(row), nil
}

// ListAPIKeysByUser returns a user's keys, without secrets.
func (s *Store) ListAPIKeysByUser(ctx context.Context, userID uuid.UUID) ([]APIKey, error) {
	rows, err := s.q.ListAPIKeysByUser(ctx, userID)
	if err != nil {
		return nil, wrap(err, "listing keys")
	}
	out := make([]APIKey, 0, len(rows))
	for _, row := range rows {
		key := apiKeyFrom(row)
		grantRows, err := s.q.ListAPIKeyGrants(ctx, key.ID)
		if err != nil {
			return nil, wrap(err, "reading key grants")
		}
		for _, g := range grantRows {
			key.Declared = append(key.Declared, authz.Grant{Role: g.Role, Scope: g.Scope})
		}
		out = append(out, key)
	}
	return out, nil
}

// RevokeAPIKey marks a key unusable, keeping the row so an audit trail can
// still name it.
func (s *Store) RevokeAPIKey(ctx context.Context, id uuid.UUID) error {
	return wrap(s.q.RevokeAPIKey(ctx, id), "revoking key %s", id)
}

// DeleteTenant removes a tenant and, by cascade, its users, their grants, and
// their keys.
//
// The cascade is the schema's, not this function's. Ownership rather than a
// filter is what makes "delete this tenant" complete without anybody writing
// the cleanup and remembering every table.
func (s *Store) DeleteTenant(ctx context.Context, id uuid.UUID) error {
	return wrap(s.q.DeleteTenant(ctx, id), "deleting tenant %s", id)
}

// SetUserStatus enables or disables a user.
//
// Disabling stops their API keys too — [ResolveAPIKey] checks the owner's
// status. Checking only the key would leave an offboarded employee's
// automation running.
func (s *Store) SetUserStatus(ctx context.Context, id uuid.UUID, status string) error {
	switch status {
	case "active", "disabled":
	default:
		return fmt.Errorf("%w: status %q is not active or disabled", ErrInvalid, status)
	}

	current, err := s.GetUser(ctx, id)
	if err != nil {
		return err
	}
	_, err = s.q.UpdateUser(ctx, dbgen.UpdateUserParams{
		ID: id, DisplayName: nilIfEmpty(current.DisplayName), Status: status,
	})
	return wrap(err, "setting status for user %s", id)
}

// SeedPlatformAdmin creates the first tenant and administrator if none exists.
//
// A deployment with no administrator is unusable and cannot fix itself: every
// operation requires a permission, and issuing the first grant requires
// role:manage. Returns the generated password exactly once, or empty if an
// administrator already existed.
func (s *Store) SeedPlatformAdmin(ctx context.Context, tenantSlug, email string) (string, error) {
	tenants, err := s.ListTenants(ctx)
	if err != nil {
		return "", err
	}
	if len(tenants) > 0 {
		// Something is already set up. Seeding again would silently mint a
		// second administrator, which is a backdoor rather than a convenience.
		return "", nil
	}

	if err := s.SeedCatalog(ctx); err != nil {
		return "", err
	}

	tenant, err := s.CreateTenant(ctx, tenantSlug, "Platform")
	if err != nil {
		return "", err
	}

	password, _, _, err := NewAPIKey()
	if err != nil {
		return "", err
	}

	admin, err := s.CreateUser(ctx, tenant.ID, email, "Platform Administrator", password)
	if err != nil {
		return "", err
	}
	if err := s.Grant(ctx, admin.ID,
		authz.Grant{Role: authz.RolePlatformAdmin, Scope: authz.GlobalScope}, nil); err != nil {
		return "", err
	}
	return password, nil
}
