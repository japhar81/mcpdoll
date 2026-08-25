// Copyright 2026 Henry Zektser.

package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mcpdoll/mcpdoll/internal/controlplane/store/dbgen"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
)

// Sessions: a person signing in to the control plane.
//
// A local password is a principal (ADR 0022). `VerifyPassword` returns a user,
// a user has grants, and grants compile to a decider — so the control plane can
// enforce exactly the model the data plane does, with no identity provider
// involved. OIDC, when it arrives, produces a `User` and joins this path rather
// than replacing it.

// DefaultSessionTTL bounds a signed-in session.
//
// Twelve hours: long enough that an operator is not re-authenticating through a
// day's work, short enough that a laptop left open overnight is not a standing
// credential. It is a ceiling, not an idle timeout — an idle timeout would sign
// somebody out mid-incident for the crime of reading.
const DefaultSessionTTL = 12 * time.Hour

// Session is a signed-in person's credential. Never carries the secret.
type Session struct {
	ID     uuid.UUID `json:"id"`
	UserID uuid.UUID `json:"user_id"`
	Prefix string    `json:"prefix"`

	UserAgent  string     `json:"user_agent,omitempty"`
	IP         string     `json:"ip,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Active reports whether this session may still authenticate.
func (s Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// SignIn verifies a password and mints a session, returning its plaintext once.
//
// Email and password. No tenant: an email identifies one person across the
// whole install, and which tenants they reach is what their grants say.
//
// Signing in *to a tenant* was a consequence of users being owned by one, and
// it was a bad trade — it meant a moved account and a wrong password failed
// identically, with no way to tell them apart from the screen.
func (s *Store) SignIn(
	ctx context.Context,
	email, password, userAgent, ip string,
) (Session, string, User, error) {
	user, err := s.VerifyPassword(ctx, email, password)
	if err != nil {
		return Session{}, "", User{}, err
	}

	plaintext, prefix, hash, err := NewAPIKey()
	if err != nil {
		return Session{}, "", User{}, err
	}

	row, err := s.q.CreateSession(ctx, dbgen.CreateSessionParams{
		UserID: user.ID, Prefix: prefix, Hash: hash,
		UserAgent: nilIfEmpty(userAgent), Ip: nilIfEmpty(ip),
		ExpiresAt: timestamptz(time.Now().Add(DefaultSessionTTL)),
	})
	if err != nil {
		return Session{}, "", User{}, wrap(err, "creating a session")
	}
	return sessionFrom(row), plaintext, user, nil
}

// ResolveSession authenticates a session token and returns its principal.
//
// Grants are read fresh on every call rather than captured at sign-in. A
// session that kept the grants it was minted with would keep them after they
// were taken away, which is the same mistake ADR 0014 refuses for API keys.
func (s *Store) ResolveSession(ctx context.Context, presented string) (Resolved, Session, error) {
	prefix, secret, err := SplitAPIKey(presented)
	if err != nil {
		return Resolved{}, Session{}, ErrNotFound
	}

	row, err := s.q.GetSessionByPrefix(ctx, prefix)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			VerifyKeySecret(secret, "")
			return Resolved{}, Session{}, ErrNotFound
		}
		return Resolved{}, Session{}, wrap(err, "reading the session")
	}
	if !VerifyKeySecret(secret, row.Hash) {
		return Resolved{}, Session{}, ErrNotFound
	}

	session := sessionFrom(row)
	if !session.Active(time.Now()) {
		return Resolved{}, Session{}, ErrNotFound
	}

	user, err := s.GetUser(ctx, session.UserID)
	if err != nil {
		return Resolved{}, Session{}, err
	}
	if user.Status != "active" {
		// Disabling a user must stop their sessions, not only their keys.
		// Checking the session row alone would leave an offboarded employee
		// signed in until it expired.
		return Resolved{}, Session{}, ErrNotFound
	}
	grants, err := s.GrantsForUser(ctx, user.ID)
	if err != nil {
		return Resolved{}, Session{}, err
	}

	// Best-effort: a failed touch must not fail the request. It is a
	// last-seen timestamp, not an authorization input.
	_ = s.q.TouchSession(ctx, session.ID)

	return Resolved{User: user, Grants: grants}, session, nil
}

// SignOut revokes one session, and records it so the data plane hears about it
// too. A control-plane session cannot call a tool, so the revocation entry is
// belt and braces — but a session id in the list costs nothing and a
// special-cased "this kind does not need revoking" is exactly the sort of
// reasoning that stops being true.
func (s *Store) SignOut(ctx context.Context, id uuid.UUID) error {
	if err := s.q.RevokeSession(ctx, id); err != nil {
		return wrap(err, "revoking the session")
	}
	_, err := s.q.AddRevocation(ctx, dbgen.AddRevocationParams{
		PrincipalID: id, Kind: "session", Reason: strPtr("signed out"),
	})
	return wrap(err, "recording the revocation")
}

// ListSessions returns a user's sessions, newest first.
func (s *Store) ListSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	rows, err := s.q.ListSessionsByUser(ctx, userID)
	if err != nil {
		return nil, wrap(err, "listing sessions")
	}
	out := make([]Session, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionFrom(row))
	}
	return out, nil
}

// Decider compiles a principal's grants against the stored role catalog.
//
// The same engine the data plane uses, so the control plane cannot drift into a
// second authorization model. `authz`'s conformance test is what keeps that
// true across engines.
func (s *Store) Decider(ctx context.Context, grants []authz.Grant) (authz.Decider, error) {
	catalog, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	return authz.BuiltinEngine{}.Prepare(ctx, grants, catalog)
}

func sessionFrom(row dbgen.Session) Session {
	out := Session{
		ID: row.ID, UserID: row.UserID, Prefix: row.Prefix,
		CreatedAt: row.CreatedAt.Time, ExpiresAt: row.ExpiresAt.Time,
	}
	if row.UserAgent != nil {
		out.UserAgent = *row.UserAgent
	}
	if row.Ip != nil {
		out.IP = *row.Ip
	}
	if row.LastSeenAt.Valid {
		t := row.LastSeenAt.Time
		out.LastSeenAt = &t
	}
	if row.RevokedAt.Valid {
		t := row.RevokedAt.Time
		out.RevokedAt = &t
	}
	return out
}

func strPtr(s string) *string { return &s }
