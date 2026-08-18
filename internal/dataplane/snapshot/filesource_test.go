// Copyright 2026 The MCPDoll Authors.

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestFileSourceLoadOnce(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	path := filepath.Join(t.TempDir(), "snapshot.pb")
	require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, signer, 1)))

	store := NewStore(3)
	src, err := NewFileSource(FileSourceOptions{Path: path, Store: store, Verifier: verifier})
	require.NoError(t, err)

	view, err := src.LoadOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), view.Version)
	require.Equal(t, int64(1), store.Version())
}

func TestNewFileSourceRequiresItsDependencies(t *testing.T) {
	_, verifier := testSigner(t, "k1")
	store := NewStore(1)

	_, err := NewFileSource(FileSourceOptions{Store: store, Verifier: verifier})
	require.ErrorContains(t, err, "needs a path")

	_, err = NewFileSource(FileSourceOptions{Path: "x", Verifier: verifier})
	require.ErrorContains(t, err, "needs a store")

	// The verifier is not optional for a file source either: the transport was
	// never the security boundary.
	_, err = NewFileSource(FileSourceOptions{Path: "x", Store: store})
	require.ErrorContains(t, err, "needs a verifier")
}

func TestFileSourceLoadOnceErrors(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	dir := t.TempDir()
	store := NewStore(3)

	t.Run("missing file", func(t *testing.T) {
		src, err := NewFileSource(FileSourceOptions{
			Path: filepath.Join(dir, "absent.pb"), Store: store, Verifier: verifier,
		})
		require.NoError(t, err)
		_, err = src.LoadOnce(context.Background())
		require.ErrorContains(t, err, "stat")
	})

	t.Run("not a snapshot", func(t *testing.T) {
		path := filepath.Join(dir, "garbage.pb")
		require.NoError(t, os.WriteFile(path, []byte("not protobuf at all, really quite long"), 0o600))
		src, err := NewFileSource(FileSourceOptions{Path: path, Store: store, Verifier: verifier})
		require.NoError(t, err)
		_, err = src.LoadOnce(context.Background())
		require.Error(t, err)
	})

	t.Run("wrong signing key", func(t *testing.T) {
		attacker, _ := testSigner(t, "attacker")
		path := filepath.Join(dir, "untrusted.pb")
		require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, attacker, 1)))
		src, err := NewFileSource(FileSourceOptions{Path: path, Store: store, Verifier: verifier})
		require.NoError(t, err)
		_, err = src.LoadOnce(context.Background())
		var untrusted *ErrUntrustedKey
		require.ErrorAs(t, err, &untrusted,
			"a file source must verify signatures exactly as the stream does")
	})

	// Sanity: the same path with a trusted key does load, so the failures above
	// are about the key and not about the file source being broken.
	path := filepath.Join(dir, "good.pb")
	require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, signer, 1)))
	src, err := NewFileSource(FileSourceOptions{Path: path, Store: store, Verifier: verifier})
	require.NoError(t, err)
	_, err = src.LoadOnce(context.Background())
	require.NoError(t, err)
}

// TestFileSourceReloadsOnChange is the point of the source: replacing the file
// republishes, with no restart.
func TestFileSourceReloadsOnChange(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	path := filepath.Join(t.TempDir(), "snapshot.pb")
	require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, signer, 1)))

	store := NewStore(3)
	src, err := NewFileSource(FileSourceOptions{
		Path: path, Store: store, Verifier: verifier,
		Interval: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	_, err = src.LoadOnce(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx) }()

	require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, signer, 2)))
	require.Eventually(t, func() bool { return store.Version() == 2 },
		2*time.Second, 20*time.Millisecond,
		"replacing the file should republish")
}

// TestFileSourceKeepsServingOnBadFile: an operator who writes a broken file gets
// a stale-but-working gateway, not an outage.
func TestFileSourceKeepsServingOnBadFile(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	path := filepath.Join(t.TempDir(), "snapshot.pb")
	require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, signer, 5)))

	store := NewStore(3)
	src, err := NewFileSource(FileSourceOptions{
		Path: path, Store: store, Verifier: verifier,
		Interval: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	_, err = src.LoadOnce(context.Background())
	require.NoError(t, err)
	good := store.Current()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx) }()

	// Garbage.
	require.NoError(t, os.WriteFile(path, []byte("corrupt file contents here"), 0o600))
	time.Sleep(120 * time.Millisecond)
	require.Same(t, good, store.Current(), "a corrupt file must not disturb what is served")

	// A correctly-signed but structurally broken snapshot.
	snap, err := defaultFixture(6).b.Build()
	require.NoError(t, err)
	snap.Audiences[0].BundleIds = []string{"bnd_missing"}
	broken, err := signer.Sign(snap)
	require.NoError(t, err)
	require.NoError(t, WriteSignedSnapshot(path, broken))
	time.Sleep(120 * time.Millisecond)
	require.Same(t, good, store.Current(), "an unservable snapshot must not displace a working one")

	// And recovery: a good file after a bad one is picked up.
	require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, signer, 7)))
	require.Eventually(t, func() bool { return store.Version() == 7 },
		2*time.Second, 20*time.Millisecond,
		"the source must recover once the file is valid again")
}

// TestFileSourceIgnoresStaleRewrite: re-touching the same version must not churn.
func TestFileSourceIgnoresStaleRewrite(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	path := filepath.Join(t.TempDir(), "snapshot.pb")
	signed := signedFixture(t, signer, 3)
	require.NoError(t, WriteSignedSnapshot(path, signed))

	store := NewStore(3)
	var activations int
	store.Observe(func(*View) { activations++ })

	src, err := NewFileSource(FileSourceOptions{
		Path: path, Store: store, Verifier: verifier,
		Interval: 20 * time.Millisecond,
	})
	require.NoError(t, err)
	_, err = src.LoadOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, activations)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx) }()

	// Rewrite the same version with a fresh mtime.
	time.Sleep(30 * time.Millisecond)
	require.NoError(t, WriteSignedSnapshot(path, signed))
	time.Sleep(120 * time.Millisecond)

	require.Equal(t, 1, activations,
		"re-writing the same version must not cause a second activation")
	require.Equal(t, int64(3), store.Version())
}

// TestWriteSignedSnapshotIsAtomic: a reader polling for changes must never see a
// partially written file.
func TestWriteSignedSnapshotIsAtomic(t *testing.T) {
	signer, _ := testSigner(t, "k1")
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.pb")
	signed := signedFixture(t, signer, 1)
	require.NoError(t, WriteSignedSnapshot(path, signed))

	// No temp files survive a successful write.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the temp file should have been renamed, not left behind")
	require.Equal(t, "snapshot.pb", entries[0].Name())

	got, err := ReadSignedSnapshot(path)
	require.NoError(t, err)
	require.True(t, proto.Equal(signed, got))
}

func TestWriteSignedSnapshotCreatesDirectories(t *testing.T) {
	signer, _ := testSigner(t, "k1")
	path := filepath.Join(t.TempDir(), "nested", "deeper", "snapshot.pb")
	require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, signer, 1)))
	_, err := ReadSignedSnapshot(path)
	require.NoError(t, err)
}

func TestReadSignedSnapshotErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadSignedSnapshot(filepath.Join(dir, "absent.pb"))
	require.ErrorContains(t, err, "reading")

	bad := filepath.Join(dir, "bad.pb")
	require.NoError(t, os.WriteFile(bad, []byte("this is definitely not protobuf"), 0o600))
	_, err = ReadSignedSnapshot(bad)
	require.ErrorContains(t, err, "parsing")
}

func TestFileSourceRunStopsOnContextCancel(t *testing.T) {
	signer, verifier := testSigner(t, "k1")
	path := filepath.Join(t.TempDir(), "snapshot.pb")
	require.NoError(t, WriteSignedSnapshot(path, signedFixture(t, signer, 1)))

	store := NewStore(1)
	src, err := NewFileSource(FileSourceOptions{
		Path: path, Store: store, Verifier: verifier, Interval: 10 * time.Millisecond,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- src.Run(ctx) }()
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}
