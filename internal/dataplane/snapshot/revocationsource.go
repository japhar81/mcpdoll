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

// RevocationSource polls a file for a signed revocation list.
//
// Deliberately the same shape as [FileSource], including the polling: an
// operator replaces this file the same ways they replace a snapshot — atomic
// rename, in-place write, a ConfigMap symlink swap — and a second distribution
// mechanism would be a second thing to operate for no gain.
//
// It polls faster than the snapshot source. The whole point of this artifact is
// that a leaked credential must not wait, and the interval is the floor on how
// long it does wait.
type RevocationSource struct {
	path     string
	store    *Store
	verifier *Verifier
	log      *slog.Logger
	interval time.Duration

	lastModTime time.Time
	lastSize    int64

	// onApply is notified after a list is accepted, so metrics and the edge's
	// principal cache can react to it.
	onApply func(*Revocations)
}

// RevocationSourceOptions configures a [RevocationSource].
type RevocationSourceOptions struct {
	Path     string
	Store    *Store
	Verifier *Verifier
	Logger   *slog.Logger
	// Interval between checks. Defaults to 2s — faster than the snapshot's 5s,
	// because this artifact's reason for existing is that it must not wait.
	Interval time.Duration
	OnApply  func(*Revocations)
}

// NewRevocationSource builds a file-backed revocation source.
func NewRevocationSource(opts RevocationSourceOptions) (*RevocationSource, error) {
	if opts.Path == "" {
		return nil, errors.New("snapshot: revocation source needs a path")
	}
	if opts.Store == nil {
		return nil, errors.New("snapshot: revocation source needs a store")
	}
	if opts.Verifier == nil {
		return nil, errors.New("snapshot: revocation source needs a verifier; " +
			"an unsigned revocation list is a denial-of-service primitive, since " +
			"anyone who could write the file could revoke every principal")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Interval <= 0 {
		opts.Interval = 2 * time.Second
	}
	return &RevocationSource{
		path: opts.Path, store: opts.Store, verifier: opts.Verifier,
		log: opts.Logger, interval: opts.Interval, onApply: opts.OnApply,
	}, nil
}

// LoadOnce reads and applies the file exactly once.
//
// An absent file is not an error. A deployment that has never revoked anything
// has nothing to distribute, and refusing to start over it would make the
// safety mechanism a liveness risk.
func (r *RevocationSource) LoadOnce(ctx context.Context) error {
	signed, info, err := r.read()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.log.InfoContext(ctx, "no revocation list yet", "path", r.path)
			return nil
		}
		return err
	}
	list, err := r.verifier.VerifyRevocations(signed)
	if err != nil {
		return err
	}
	if err := r.store.ApplyRevocations(list); err != nil {
		return err
	}
	r.lastModTime, r.lastSize = info.ModTime(), info.Size()
	r.log.InfoContext(ctx, "loaded revocation list",
		"path", r.path, "version", list.Version, "revoked", list.Count())
	if r.onApply != nil {
		r.onApply(list)
	}
	return nil
}

// Run polls until ctx is cancelled.
//
// A failure never stops the loop and never disturbs what is in effect. The
// previous list stays, which is the correct failure: refusing to serve because
// the revocation list is unreachable would let a control-plane outage stop tool
// calls, reversing the property the whole architecture provides (ADR 0002).
//
// The cost of that choice is the exposure ADR 0023 does not eliminate, and it
// is measurable rather than hidden: [Revocations.Age] is how long a leaked
// credential would keep working.
func (r *RevocationSource) Run(ctx context.Context) error {
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

func (r *RevocationSource) checkOnce(ctx context.Context) {
	info, err := os.Stat(r.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.log.WarnContext(ctx, "cannot stat the revocation list",
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
		r.log.WarnContext(ctx, "cannot read the revocation list", "path", r.path, "err", err)
		return
	}
	list, err := r.verifier.VerifyRevocations(signed)
	if err != nil {
		r.log.ErrorContext(ctx, "refusing an unverifiable revocation list",
			"path", r.path, "err", err)
		return
	}
	if err := r.store.ApplyRevocations(list); err != nil {
		switch {
		case errors.Is(err, ErrStaleRevocations):
			// Ordinary: the file was touched without changing.
			r.log.DebugContext(ctx, "revocation list is not newer", "version", list.Version)
		case errors.Is(err, ErrRevocationsAheadOfSnapshot):
			// Worth a warning rather than a debug line: it means the snapshot
			// this data plane is serving is behind, which is an operational
			// fact somebody should see.
			r.log.WarnContext(ctx, "revocation list is ahead of the serving snapshot",
				"pruned_through", list.PrunedThroughVersion, "serving", r.store.Version())
		default:
			r.log.ErrorContext(ctx, "cannot apply the revocation list", "err", err)
		}
		return
	}
	r.log.InfoContext(ctx, "applied revocation list",
		"version", list.Version, "revoked", list.Count())
	if r.onApply != nil {
		r.onApply(list)
	}
}

func (r *RevocationSource) read() (*snapshotpb.SignedRevocationList, os.FileInfo, error) {
	info, err := os.Stat(r.path)
	if err != nil {
		return nil, nil, err
	}
	signed, err := ReadSignedRevocations(r.path)
	if err != nil {
		return nil, nil, err
	}
	return signed, info, nil
}
