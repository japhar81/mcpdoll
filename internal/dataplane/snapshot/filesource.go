// Copyright 2026 The MCPDoll Authors.

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// FileSource loads snapshots from a file on disk and re-activates when it
// changes.
//
// This is not a development convenience. It is the mechanism for the deployment
// the whole signing design exists to support: a data plane in an air-gapped
// network, or one whose control plane it does not trust, fed a signed artifact
// out of band. Because the signature is verified either way, the file source is
// exactly as safe as the gRPC stream — the transport was never the security
// boundary.
//
// Change detection is by modification time and size rather than a watcher.
// Polling a single file every few seconds costs nothing, and inotify-style
// watchers are unreliable across the ways an operator actually replaces a file:
// atomic rename, in-place write, a Kubernetes ConfigMap symlink swap.
type FileSource struct {
	path     string
	store    *Store
	verifier *Verifier
	log      *slog.Logger
	interval time.Duration

	lastModTime time.Time
	lastSize    int64
}

// FileSourceOptions configures a [FileSource].
type FileSourceOptions struct {
	Path     string
	Store    *Store
	Verifier *Verifier
	Logger   *slog.Logger
	// Interval between change checks. Defaults to 5s.
	Interval time.Duration
}

// NewFileSource builds a file-backed snapshot source.
func NewFileSource(opts FileSourceOptions) (*FileSource, error) {
	if opts.Path == "" {
		return nil, errors.New("snapshot: file source needs a path")
	}
	if opts.Store == nil {
		return nil, errors.New("snapshot: file source needs a store")
	}
	if opts.Verifier == nil {
		return nil, errors.New("snapshot: file source needs a verifier; " +
			"an unverified snapshot is never activated, whatever the transport")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Second
	}
	return &FileSource{
		path:     opts.Path,
		store:    opts.Store,
		verifier: opts.Verifier,
		log:      opts.Logger,
		interval: opts.Interval,
	}, nil
}

// LoadOnce reads and activates the file exactly once.
//
// Called at startup so the process is either ready or has failed for a legible
// reason, rather than starting and 503-ing until a poll happens to succeed.
func (f *FileSource) LoadOnce(ctx context.Context) (*View, error) {
	signed, info, err := f.read()
	if err != nil {
		return nil, err
	}
	view, err := f.store.Activate(signed, f.verifier)
	if err != nil {
		return nil, err
	}
	f.lastModTime = info.ModTime()
	f.lastSize = info.Size()
	f.log.InfoContext(ctx, "loaded snapshot from file",
		"path", f.path, "version", view.Version)
	return view, nil
}

// Run polls for changes until ctx is cancelled.
//
// A failed reload is logged and retried on the next tick; it never stops the
// loop and never disturbs what is being served. An operator who writes a bad
// file gets a log line and a stale-but-working gateway, which is the correct
// outcome.
func (f *FileSource) Run(ctx context.Context) error {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			f.checkOnce(ctx)
		}
	}
}

func (f *FileSource) checkOnce(ctx context.Context) {
	info, err := os.Stat(f.path)
	if err != nil {
		f.log.WarnContext(ctx, "cannot stat snapshot file", "path", f.path, "err", err)
		return
	}
	if info.ModTime().Equal(f.lastModTime) && info.Size() == f.lastSize {
		return
	}

	signed, freshInfo, err := f.read()
	if err != nil {
		f.log.ErrorContext(ctx, "cannot read snapshot file", "path", f.path, "err", err)
		return
	}
	// Record the stat even on a failed activation, so a persistently bad file is
	// not re-read and re-logged on every tick.
	f.lastModTime = freshInfo.ModTime()
	f.lastSize = freshInfo.Size()

	view, err := f.store.Activate(signed, f.verifier)
	if err != nil {
		var stale *ErrStaleVersion
		if errors.As(err, &stale) {
			// Not an error worth alarming about: an operator re-touched the
			// file, or the same snapshot was rewritten.
			f.log.DebugContext(ctx, "snapshot file is not newer than what is serving",
				"offered", stale.Offered, "serving", stale.Serving)
			return
		}
		f.log.ErrorContext(ctx, "refused snapshot from file",
			"path", f.path, "err", err)
		return
	}
	f.log.InfoContext(ctx, "activated snapshot from file",
		"path", f.path, "version", view.Version)
}

func (f *FileSource) read() (*snapshotpb.SignedSnapshot, os.FileInfo, error) {
	info, err := os.Stat(f.path)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot: stat %s: %w", f.path, err)
	}
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot: reading %s: %w", f.path, err)
	}
	var signed snapshotpb.SignedSnapshot
	if err := proto.Unmarshal(raw, &signed); err != nil {
		return nil, nil, fmt.Errorf("snapshot: parsing %s: %w", f.path, err)
	}
	return &signed, info, nil
}

// WriteSignedSnapshot serializes a signed snapshot to disk atomically.
//
// Write-to-temp-then-rename, because a reader polling for changes must never
// observe a half-written file. Without the rename, a large snapshot would
// reliably produce a "parsing: unexpected EOF" on whichever poll caught it
// mid-write.
func WriteSignedSnapshot(path string, signed *snapshotpb.SignedSnapshot) error {
	raw, err := proto.Marshal(signed)
	if err != nil {
		return fmt.Errorf("snapshot: serializing: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("snapshot: creating %s: %w", dir, err)
	}
	// Same directory, so the rename is on one filesystem and therefore atomic.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("snapshot: creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("snapshot: renaming into place: %w", err)
	}
	return nil
}

// ReadSignedSnapshot loads a signed snapshot from disk without activating it,
// for CLI inspection.
func ReadSignedSnapshot(path string) (*snapshotpb.SignedSnapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot: reading %s: %w", path, err)
	}
	var signed snapshotpb.SignedSnapshot
	if err := proto.Unmarshal(raw, &signed); err != nil {
		return nil, fmt.Errorf("snapshot: parsing %s: %w", path, err)
	}
	return &signed, nil
}
