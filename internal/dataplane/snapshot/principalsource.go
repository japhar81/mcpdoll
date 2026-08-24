// Copyright 2026 Henry Zektser.

package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// PrincipalSource polls a file for a signed principal set.
//
// The same shape as [FileSource] and [RevocationSource]: an operator replaces
// these files the same ways, and a third distribution mechanism would be a
// third thing to operate for no gain.
//
// It polls faster than the snapshot source, because the point of separating it
// is that minting a key or issuing a grant should be usable in about a second
// rather than at a publish (ADR 0024). The interval is the floor on that.
type PrincipalSource struct {
	path     string
	store    *Store
	verifier *Verifier
	log      *slog.Logger
	interval time.Duration

	lastModTime time.Time
	lastSize    int64

	// onApply is notified after a set is accepted, so metrics and the edge's
	// principal cache can react to it.
	onApply func(*Principals)
}

// PrincipalSourceOptions configures a [PrincipalSource].
type PrincipalSourceOptions struct {
	Path     string
	Store    *Store
	Verifier *Verifier
	Logger   *slog.Logger
	// Interval between checks. Defaults to 2s — faster than the snapshot's 5s,
	// because this artifact's reason for existing is that it must not wait.
	Interval time.Duration
	OnApply  func(*Principals)
}

// NewPrincipalSource builds a file-backed principal source.
func NewPrincipalSource(opts PrincipalSourceOptions) (*PrincipalSource, error) {
	if opts.Path == "" {
		return nil, errors.New("snapshot: principal source needs a path")
	}
	if opts.Store == nil {
		return nil, errors.New("snapshot: principal source needs a store")
	}
	if opts.Verifier == nil {
		return nil, errors.New("snapshot: principal source needs a verifier; " +
			"an unsigned principal set is a denial-of-service primitive, since " +
			"anyone who could write the file could revoke every principal")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	return &PrincipalSource{
		path: opts.Path, store: opts.Store, verifier: opts.Verifier,
		log: opts.Logger, interval: opts.Interval, onApply: opts.OnApply,
	}, nil
}

// LoadOnce reads and applies the file exactly once.
//
// An absent file is not an error. A gateway with no principal set serves
// nobody, which is a legible state — an install where nothing has been
// published yet — rather than a reason to refuse to start.
func (r *PrincipalSource) LoadOnce(ctx context.Context) error {
	signed, info, err := r.read()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.log.InfoContext(ctx, "no principal set yet", "path", r.path)
			return nil
		}
		return err
	}
	set, err := r.verifier.VerifyPrincipals(signed)
	if err != nil {
		return err
	}
	if err := r.store.ApplyPrincipals(set); err != nil {
		return err
	}
	r.lastModTime, r.lastSize = info.ModTime(), info.Size()
	r.log.InfoContext(ctx, "loaded principal set",
		"path", r.path, "version", set.Version, "principals", set.Count())
	if r.onApply != nil {
		r.onApply(set)
	}
	return nil
}

// Run polls until ctx is cancelled.
//
// A failure never stops the loop and never disturbs what is in effect. The
// previous set stays, which is the correct failure: refusing to serve because
// the principal set is unreachable would let a control-plane outage stop tool
// calls, reversing the property the whole architecture provides (ADR 0002).
//
// The previous set stays, which is the correct failure: refusing to serve
// because the set is unreachable would let a control-plane outage stop tool
// calls (ADR 0002). [Principals.Age] measures how far behind it is.
func (r *PrincipalSource) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.checkOnce(ctx)
		}
	}
}

func (r *PrincipalSource) checkOnce(ctx context.Context) {
	info, err := os.Stat(r.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.log.WarnContext(ctx, "cannot stat the principal set",
				"path", r.path, "err", err)
		}
		return
	}
	if info.ModTime().Equal(r.lastModTime) && info.Size() == r.lastSize {
		return
	}

	// Record what was seen before attempting the load. A file that fails to
	// verify must not be retried every tick — that would fill the log with the
	// same line forever and hide the next real change.
	r.lastModTime, r.lastSize = info.ModTime(), info.Size()

	signed, _, err := r.read()
	if err != nil {
		r.log.WarnContext(ctx, "cannot read the principal set", "path", r.path, "err", err)
		return
	}
	set, err := r.verifier.VerifyPrincipals(signed)
	if err != nil {
		r.log.ErrorContext(ctx, "refusing an unverifiable principal set",
			"path", r.path, "err", err)
		return
	}
	if err := r.store.ApplyPrincipals(set); err != nil {
		switch {
		case errors.Is(err, ErrStalePrincipals):
			// Ordinary: the file was touched without changing.
			r.log.DebugContext(ctx, "principal set is not newer", "version", set.Version)
		default:
			r.log.ErrorContext(ctx, "cannot apply the principal set", "err", err)
		}
		return
	}
	r.log.InfoContext(ctx, "applied principal set",
		"version", set.Version, "principals", set.Count())
	if r.onApply != nil {
		r.onApply(set)
	}
}

func (r *PrincipalSource) read() (*snapshotpb.SignedPrincipalSet, os.FileInfo, error) {
	info, err := os.Stat(r.path)
	if err != nil {
		return nil, nil, err
	}
	signed, err := ReadSignedPrincipals(r.path)
	if err != nil {
		return nil, nil, err
	}
	return signed, info, nil
}
