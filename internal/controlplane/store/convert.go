// Copyright 2026 Henry Zektser.

package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/store/dbgen"
)

// wrap turns a driver error into one of this package's sentinels.
//
// Callers branch on ErrNotFound and ErrConflict to choose an HTTP status or an
// exit code. Leaking a pgx error would make every one of those call sites
// depend on the driver, and a driver upgrade could change what they mean.
func wrap(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	context := fmt.Sprintf(format, args...)

	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", context, ErrNotFound)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%s: %w", context, ErrConflict)
		case "23503": // foreign_key_violation
			// The referenced row is gone, which from the caller's side is the
			// same situation as it never having existed.
			return fmt.Errorf("%s: %w (a referenced row does not exist)", context, ErrNotFound)
		case "23514": // check_violation
			return fmt.Errorf("%s: %w (%s)", context, ErrInvalid, pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("%s: %w", context, err)
}

// validateSlug enforces what a scope string can safely contain.
//
// A slug appears inside `t/<slug>/ts/...`, so a slug containing a slash would
// change the meaning of every scope built from it — `t/a/ts/x` from a tenant
// named `a/ts/x` is indistinguishable from a toolset scope. That is a
// privilege boundary, not a formatting preference.
func validateSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("%w: a slug is required", ErrInvalid)
	}
	if len(slug) > 63 {
		return fmt.Errorf("%w: slug %q is longer than 63 characters", ErrInvalid, slug)
	}
	if slug == "*" {
		return fmt.Errorf("%w: %q is the global scope and cannot be a tenant slug",
			ErrInvalid, slug)
	}
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf(
				"%w: slug %q may contain only lowercase letters, digits, hyphen, "+
					"and underscore — it becomes part of every authorization scope",
				ErrInvalid, slug)
		}
	}
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") {
		return fmt.Errorf("%w: slug %q may not begin or end with a hyphen", ErrInvalid, slug)
	}
	return nil
}

// ----------------------------------------------------------------- mapping --

func tenantFrom(row dbgen.Tenant) Tenant {
	return Tenant{
		ID: row.ID, Slug: row.Slug, Name: row.Name, Status: row.Status,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func userFrom(row dbgen.User) User {
	return User{
		ID: row.ID, TenantID: row.TenantID, Email: row.Email,
		DisplayName: derefOrEmpty(row.DisplayName),
		Status:      row.Status,
		// The hash itself never leaves this package.
		HasPassword: row.PasswordHash != nil && *row.PasswordHash != "",
		CreatedAt:   row.CreatedAt.Time,
	}
}

func apiKeyFrom(row dbgen.ApiKey) APIKey {
	return APIKey{
		ID: row.ID, UserID: row.UserID, Name: row.Name, Prefix: row.Prefix,
		CreatedAt:  row.CreatedAt.Time,
		LastUsedAt: timePtr(row.LastUsedAt),
		ExpiresAt:  timePtr(row.ExpiresAt),
		RevokedAt:  timePtr(row.RevokedAt),
	}
}

func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// timestamptz converts a required time. Distinct from [timestamptzPtr] because
// a column that is NOT NULL and one that is nullable want different zero
// behaviour, and conflating them writes a NULL where the schema forbids it.
func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func timestamptzPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
