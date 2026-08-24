// Copyright 2026 The MCPDoll Authors.

package health

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// fakeLister stands in for the backend pool.
//
// A real pool would need five HTTP servers to exercise one state transition,
// and the transitions are the thing under test here — the end-to-end path
// against live fixtures is covered in internal/dataplane/edge.
type fakeLister struct {
	mu      sync.Mutex
	tools   []*sdk.Tool
	err     error
	calls   int
	version string
}

func (f *fakeLister) ListTools(context.Context, string, backends.Principal) ([]*sdk.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.tools, nil
}

func (f *fakeLister) NegotiatedVersion(string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version
}

func (f *fakeLister) set(tools []*sdk.Tool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools, f.err = tools, err
}

func (f *fakeLister) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func checkStock() *sdk.Tool {
	return &sdk.Tool{
		Name:        "check_stock",
		Title:       "Check stock",
		Description: "Report how many units of a SKU are on hand.",
		InputSchema: rawSchema(`{"type":"object","properties":{"sku":{"type":"string"}}}`),
	}
}

func rawSchema(s string) any {
	// json.RawMessage, matching what the adapter produces. A []byte here would
	// base64-encode, which the SDK rejects — a mistake this codebase has
	// already made once.
	return json.RawMessage(s)
}

// newStoreWithSnapshot activates a snapshot holding one backend and one tool.
func newStoreWithSnapshot(t *testing.T, servingMode snapshotpb.ServingMode) *snapshot.Store {
	t.Helper()

	pub, priv, err := snapshot.GenerateKey()
	require.NoError(t, err)
	signer, err := snapshot.NewSigner("k", priv)
	require.NoError(t, err)
	verifier, err := snapshot.NewVerifierFromKeys(map[string]ed25519.PublicKey{"k": pub})
	require.NoError(t, err)

	b := snapshot.NewBuilder(1).
		WithID("snap_1").
		WithCatalogDefaults(5*time.Minute, 30*time.Second)
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_whs", Name: "warehouse", Prefix: "whs"})
	b.AddServer(&snapshotpb.Server{
		Id: "srv_whs", Name: "warehouse", NamespaceId: "ns_whs",
		Endpoint: "http://127.0.0.1:1", ServingMode: servingMode,
	})
	// The admitted definition is built from the same tool the healthy fake
	// returns, so "no drift" means the whole canonicalize-digest-compare path
	// agreed rather than that the test asserted its own inputs match.
	admitted := checkStock()
	b.AddTool(snapshot.ToolInput{
		ServerID: "srv_whs", NamespaceID: "ns_whs", Prefix: "whs",
		Name: admitted.Name, Title: admitted.Title, Description: admitted.Description,
		InputSchema: admitted.InputSchema.(json.RawMessage),
		EffectClass: snapshotpb.EffectClass_EFFECT_CLASS_READ,
	})
	b.AddBundle(&snapshotpb.Bundle{
		Id: "bnd", Name: "b", Priority: 10,
		Entries: []*snapshotpb.BundleEntry{{NamespaceId: "ns_whs"}},
	})
	b.AddAudience(&snapshotpb.Audience{Id: "aud", Slug: "a", Name: "A", BundleIds: []string{"bnd"}})

	snap, err := b.Build()
	require.NoError(t, err)
	signed, err := signer.Sign(snap)
	require.NoError(t, err)

	store := snapshot.NewStore(3)
	_, err = store.Activate(signed, verifier)
	require.NoError(t, err)
	return store
}

func newProber(t *testing.T, lister Lister, store *snapshot.Store) *Prober {
	t.Helper()
	return New(Options{
		Pool:        lister,
		Snapshot:    store,
		Registry:    NewRegistry(),
		GraceWindow: time.Minute,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestAHealthyBackendProducesNoDrift(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{tools: []*sdk.Tool{checkStock()}, version: "2026-07-28"}
	p := newProber(t, lister, newStoreWithSnapshot(t, snapshotpb.ServingMode_SERVING_MODE_STRICT))

	p.ProbeAll(context.Background())

	b, ok := p.Registry().Backend("srv_whs")
	require.True(t, ok)
	require.Equal(t, StateHealthy, b.State)
	require.Empty(t, b.Drift)
	require.Equal(t, "2026-07-28", b.NegotiatedVersion)
	require.Equal(t, 1, b.ToolsAdmitted)
	require.Equal(t, 1, b.ToolsObserved)
	require.Empty(t, p.Registry().Summary().BlockedTools)
}

func TestAFailingBackendIsDegradedThenUnavailable(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{tools: []*sdk.Tool{checkStock()}}
	store := newStoreWithSnapshot(t, snapshotpb.ServingMode_SERVING_MODE_STRICT)

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	p := New(Options{
		Pool: lister, Snapshot: store, Registry: NewRegistry(),
		GraceWindow: 10 * time.Minute,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         func() time.Time { return now },
	})

	p.ProbeAll(context.Background())
	b, _ := p.Registry().Backend("srv_whs")
	require.Equal(t, StateHealthy, b.State)

	lister.set(nil, errors.New("connection refused"))

	now = now.Add(2 * time.Minute)
	p.ProbeAll(context.Background())
	b, _ = p.Registry().Backend("srv_whs")
	require.Equal(t, StateDegraded, b.State)
	require.Equal(t, 1, b.ConsecutiveFailures)
	require.Contains(t, b.Error, "connection refused")

	now = now.Add(20 * time.Minute)
	p.ProbeAll(context.Background())
	b, _ = p.Registry().Backend("srv_whs")
	require.Equal(t, StateUnavailable, b.State)
	require.Equal(t, 2, b.ConsecutiveFailures)
}

func TestDriftSurvivesABackendGoingDown(t *testing.T) {
	t.Parallel()
	drifted := checkStock()
	drifted.InputSchema = rawSchema(`{"type":"object","properties":{"sku":{"type":"string"},"wh":{"type":"string"}},"required":["wh"]}`)

	lister := &fakeLister{tools: []*sdk.Tool{drifted}}
	p := newProber(t, lister, newStoreWithSnapshot(t, snapshotpb.ServingMode_SERVING_MODE_STRICT))

	p.ProbeAll(context.Background())
	b, _ := p.Registry().Backend("srv_whs")
	require.Equal(t, StateDrifted, b.State)
	require.Equal(t, []string{"whs.check_stock"}, b.BlockedTools())

	// Now it stops answering. A backend that is down has not stopped being
	// drifted, and clearing the drift here would silently unblock a tool that
	// strict mode was correctly refusing.
	lister.set(nil, errors.New("connection refused"))
	p.ProbeAll(context.Background())

	b, _ = p.Registry().Backend("srv_whs")
	// Degraded rather than unavailable: it succeeded moments ago, so it is
	// inside its grace window. Which failure state it is in does not matter
	// here — the block must hold in either.
	require.Equal(t, StateDegraded, b.State)
	require.Equal(t, []string{"whs.check_stock"}, b.BlockedTools())
	_, blocked := p.Registry().Blocked("whs.check_stock")
	require.True(t, blocked)
}

func TestAdvisoryModeRecordsDriftWithoutBlocking(t *testing.T) {
	t.Parallel()
	drifted := checkStock()
	drifted.InputSchema = rawSchema(`{"type":"object","properties":{"other":{"type":"number"}}}`)

	lister := &fakeLister{tools: []*sdk.Tool{drifted}}
	p := newProber(t, lister, newStoreWithSnapshot(t, snapshotpb.ServingMode_SERVING_MODE_ADVISORY))

	p.ProbeAll(context.Background())
	b, _ := p.Registry().Backend("srv_whs")

	// The drift is seen and reported; the tool keeps serving. That difference
	// is the entire content of the advisory setting.
	require.Equal(t, StateDrifted, b.State)
	require.Len(t, b.Drift, 1)
	require.Equal(t, DriftSemantic, b.Drift[0].Kind)
	require.Empty(t, b.BlockedTools())
	require.Equal(t, 0, p.Registry().Summary().BlockedTools)
}

func TestCosmeticDriftNeverBlocksEvenUnderStrict(t *testing.T) {
	t.Parallel()
	reworded := checkStock()
	reworded.Description = "Reports on-hand units. Rewritten by the docs team."

	lister := &fakeLister{tools: []*sdk.Tool{reworded}}
	p := newProber(t, lister, newStoreWithSnapshot(t, snapshotpb.ServingMode_SERVING_MODE_STRICT))

	p.ProbeAll(context.Background())
	b, _ := p.Registry().Backend("srv_whs")

	// Healthy, not drifted: the state machine only treats blocking drift as a
	// state change, so a docs edit does not page anyone.
	require.Equal(t, StateHealthy, b.State)
	require.Len(t, b.Drift, 1)
	require.Equal(t, DriftCosmetic, b.Drift[0].Kind)
	require.Empty(t, b.BlockedTools())
}

func TestProbeAllDoesNothingWithoutASnapshot(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{tools: []*sdk.Tool{checkStock()}}
	p := newProber(t, lister, snapshot.NewStore(3))

	p.ProbeAll(context.Background())

	// Nothing is being served, so nothing's fitness to serve is in question —
	// and probing endpoints from a snapshot that was never activated would
	// generate traffic to backends this gateway is not fronting.
	require.Zero(t, lister.callCount())
	require.Empty(t, p.Registry().All())
}

func TestRunProbesImmediatelyRatherThanAfterOneInterval(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{tools: []*sdk.Tool{checkStock()}}
	store := newStoreWithSnapshot(t, snapshotpb.ServingMode_SERVING_MODE_STRICT)

	p := New(Options{
		Pool: lister, Snapshot: store, Registry: NewRegistry(),
		// Long enough that a second sweep cannot happen during this test.
		Interval: time.Hour,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	require.Eventually(t, func() bool {
		_, ok := p.Registry().Backend("srv_whs")
		return ok
	}, 2*time.Second, 10*time.Millisecond,
		"a gateway that has just started should learn what it is fronting now, "+
			"not after one interval")

	cancel()
	<-done
}
