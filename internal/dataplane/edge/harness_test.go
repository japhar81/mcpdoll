// Copyright 2026 Henry Zektser.

package edge_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/fixtures"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/health"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	mcpadapter "github.com/mcpdoll/mcpdoll/internal/mcp"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// harness wires a complete data plane in-process: real fixture backends, a real
// signed snapshot built from what those backends actually publish, a real
// backend pool, and the real edge behind a real HTTP listener.
//
// Nothing here is mocked. The point of the conformance suite is to catch
// disagreements between our understanding of the protocol and the SDK's, and a
// mock would agree with whichever one wrote it.
type harness struct {
	t *testing.T

	Modern      *fixtures.Backend
	Legacy      *fixtures.Backend
	Misbehaving *fixtures.MisbehavingBackend
	Hostile     *fixtures.Backend
	Confirming  *fixtures.Backend

	Store    *snapshot.Store
	Pool     *backends.Pool
	Prober   *health.Prober
	Edge     *edge.Edge
	Signer   *snapshot.Signer
	Verifier *snapshot.Verifier

	Server *httptest.Server

	version int64
}

type harnessOptions struct {
	// Pipeline installs a hook engine. Nil means pure aggregation.
	Pipeline edge.Pipeline
	// WithStateSigner enables MRTR requestState signing.
	WithStateSigner bool
	// SkipHostile leaves the hostile backend out of the catalog for tests that
	// only care about well-behaved traffic.
	SkipHostile bool
	// Verbose logs the data plane to the test output, for diagnosing a failure.
	Verbose bool
	// PoolTransport wraps the pool's outbound HTTP transport, for inspecting the
	// exact exchange with a backend.
	PoolTransport func(http.RoundTripper) http.RoundTripper
	// TokenSource configures per-backend credential exchange.
	TokenSource backends.TokenSource
	// NoGrants publishes the principal with no grants, for tests about an
	// empty catalog.
	NoGrants bool
	// NoDefaultSubject makes the dev identity resolver refuse an unidentified
	// request instead of defaulting to a subject.
	NoDefaultSubject bool
	// WithProber installs a health prober and wires its registry in as the
	// edge's drift guard, so a drifted tool is actually refused.
	WithProber bool
	// AdvisoryWarehouse marks the warehouse backend advisory rather than
	// strict, which is the difference between recording drift and acting on it.
	AdvisoryWarehouse bool
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	h := &harness{t: t}

	h.Modern = fixtures.NewModern()
	h.Legacy = fixtures.NewLegacy()
	h.Misbehaving = fixtures.NewMisbehaving()
	h.Hostile = fixtures.NewHostile()
	h.Confirming = fixtures.NewConfirming()

	for _, b := range []*fixtures.Backend{h.Modern, h.Legacy, h.Misbehaving.Backend, h.Hostile, h.Confirming} {
		b.Start()
		t.Cleanup(b.Close)
	}

	pub, priv, err := snapshot.GenerateKey()
	require.NoError(t, err)
	h.Signer, err = snapshot.NewSigner("test-key", priv)
	require.NoError(t, err)
	h.Verifier, err = snapshot.NewVerifierFromKeys(map[string]ed25519.PublicKey{"test-key": pub})
	require.NoError(t, err)

	h.Store = snapshot.NewStore(5)

	var logger *slog.Logger
	if opts.Verbose {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	poolOpts := backends.Options{
		Logger:           logger,
		TokenSource:      opts.TokenSource,
		FailureThreshold: 3,
		Cooldown:         200 * time.Millisecond,
		DialTimeout:      5 * time.Second,
	}
	if opts.PoolTransport != nil {
		poolOpts.HTTPClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: opts.PoolTransport(http.DefaultTransport),
		}
	}
	h.Pool = backends.New(poolOpts)
	t.Cleanup(h.Pool.Close)

	defaultSubject := "dev-user"
	if opts.NoDefaultSubject {
		defaultSubject = ""
	}
	identity, err := edge.NewHeaderIdentityResolver("test", defaultSubject, []string{"support"})
	require.NoError(t, err)

	edgeOpts := edge.Options{
		Store:    h.Store,
		Pool:     h.Pool,
		Identity: identity,
		Pipeline: opts.Pipeline,
		Logger:   logger,
	}
	if opts.WithProber {
		proberLog := logger
		if proberLog == nil {
			proberLog = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		h.Prober = health.New(health.Options{
			Pool:        h.Pool,
			Snapshot:    h.Store,
			GraceWindow: time.Minute,
			Timeout:     5 * time.Second,
			Logger:      proberLog,
		})
		edgeOpts.DriftGuard = h.Prober.Registry()
	}
	if opts.WithStateSigner {
		key, err := edge.GenerateStateKey()
		require.NoError(t, err)
		signer, err := edge.NewStateSigner(key)
		require.NoError(t, err)
		edgeOpts.StateSigner = signer
	}

	h.Edge, err = edge.New(edgeOpts)
	require.NoError(t, err)

	h.Server = httptest.NewServer(h.Edge.Handler())
	t.Cleanup(h.Server.Close)

	h.Publish(opts)
	return h
}

// Publish discovers every fixture backend's live catalog, builds a snapshot from
// it, signs it, and activates it.
//
// Discovery is a real MCP round trip against each backend. That means the
// snapshot contains exactly the schemas those backends publish, canonicalized
// through the same code path admission uses — so a canonicalization bug shows up
// here rather than only in production.
func (h *harness) Publish(opts harnessOptions) {
	h.t.Helper()
	h.version++

	b := snapshot.NewBuilder(h.version).
		WithID("snap_test").
		WithCatalogDefaults(5*time.Minute, 30*time.Second)

	b.AddTenant(&snapshotpb.Tenant{
		Id: "tn_acme", Slug: "acme", Name: "Acme", Status: "active",
	})

	type backendSpec struct {
		nsID, prefix, nsName string
		srvID, srvName       string
		endpoint             string
		effects              map[string]snapshotpb.EffectClass
		include              bool
	}

	specs := []backendSpec{
		{
			nsID: "ns_crm", prefix: "crm", nsName: "crm",
			srvID: "srv_crm", srvName: "crm-prod", endpoint: h.Modern.URL(),
			effects: map[string]snapshotpb.EffectClass{
				"lookup_customer":   snapshotpb.EffectClass_EFFECT_CLASS_READ,
				"update_customer":   snapshotpb.EffectClass_EFFECT_CLASS_WRITE,
				"list_open_tickets": snapshotpb.EffectClass_EFFECT_CLASS_READ,
			},
			include: true,
		},
		{
			nsID: "ns_hr", prefix: "hr", nsName: "hr",
			srvID: "srv_hr", srvName: "hr-legacy", endpoint: h.Legacy.URL(),
			effects: map[string]snapshotpb.EffectClass{
				"lookup_employee": snapshotpb.EffectClass_EFFECT_CLASS_READ,
				"get_org_chart":   snapshotpb.EffectClass_EFFECT_CLASS_READ,
			},
			include: true,
		},
		{
			nsID: "ns_whs", prefix: "whs", nsName: "warehouse",
			srvID: "srv_whs", srvName: "warehouse-flaky", endpoint: h.Misbehaving.URL(),
			effects: map[string]snapshotpb.EffectClass{
				"check_stock":   snapshotpb.EffectClass_EFFECT_CLASS_READ,
				"reserve_stock": snapshotpb.EffectClass_EFFECT_CLASS_WRITE,
			},
			include: true,
		},
		{
			nsID: "ns_web", prefix: "web", nsName: "websearch",
			srvID: "srv_web", srvName: "websearch-hostile", endpoint: h.Hostile.URL(),
			effects: map[string]snapshotpb.EffectClass{
				"search_web": snapshotpb.EffectClass_EFFECT_CLASS_READ,
				"fetch_page": snapshotpb.EffectClass_EFFECT_CLASS_READ,
			},
			include: !opts.SkipHostile,
		},
		{
			nsID: "ns_dep", prefix: "dep", nsName: "deploy",
			srvID: "srv_dep", srvName: "deploy-confirming", endpoint: h.Confirming.URL(),
			effects: map[string]snapshotpb.EffectClass{
				"promote_release": snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE,
			},
			include: true,
		},
	}

	for _, spec := range specs {
		if !spec.include {
			continue
		}
		b.AddNamespace(&snapshotpb.Namespace{
			Id: spec.nsID, Name: spec.nsName, Prefix: spec.prefix,
			OwnerIdpGroup: "eng-" + spec.nsName,
		})
		server := &snapshotpb.Server{
			Id: spec.srvID, Name: spec.srvName, NamespaceId: spec.nsID,
			Bindings: []*snapshotpb.Binding{
				{TenantId: "tn_acme", Primary: spec.endpoint},
			},
			ServingMode: snapshotpb.ServingMode_SERVING_MODE_STRICT,
		}
		if opts.AdvisoryWarehouse && spec.srvID == "srv_whs" {
			server.ServingMode = snapshotpb.ServingMode_SERVING_MODE_ADVISORY
		}
		if opts.TokenSource != nil {
			// The pool only exchanges when the server declares an exchange; a
			// server with none simply gets no credential.
			server.TokenExchange = &snapshotpb.TokenExchange{
				TokenEndpoint:       "https://idp.internal/token",
				Audience:            spec.srvName,
				ClientCredentialRef: "secret://mcpdoll/exchange",
			}
		}
		b.AddServer(server)

		discovered, err := mcpadapter.Discover(context.Background(), mcpadapter.DiscoverOptions{
			Endpoint: spec.endpoint,
			Timeout:  10 * time.Second,
		})
		require.NoError(h.t, err, "discovering %s", spec.srvName)
		require.NotEmpty(h.t, discovered.Tools, "%s published no tools", spec.srvName)

		for _, tool := range discovered.Tools {
			def, err := mcpadapter.ToCanonical(tool)
			require.NoError(h.t, err)
			effect, ok := spec.effects[tool.Name]
			if !ok {
				effect = snapshotpb.EffectClass_EFFECT_CLASS_READ
			}
			b.AddTool(snapshot.ToolInput{
				ServerID:     spec.srvID,
				NamespaceID:  spec.nsID,
				TenantID:     "tn_acme",
				ToolsetID:    "ts_all",
				Prefix:       spec.prefix,
				Name:         def.Name,
				Title:        def.Title,
				Description:  def.Description,
				InputSchema:  rawOrNil(def.InputSchema),
				OutputSchema: rawOrNil(def.OutputSchema),
				Annotations:  def.Annotations,
				EffectClass:  effect,
			})
		}
	}

	b.AddToolset(&snapshotpb.Toolset{
		Id: "ts_all", Name: "everything", Priority: 10,
	})

	// One principal holding the whole toolset, which reproduces what the old
	// "everything" audience gave every connecting client. Tests that care about
	// narrower grants set their own RBAC.
	grants := []*snapshotpb.Grant{
		{Role: authz.RoleToolUser, Scope: authz.ToolsetScope("acme", "everything")},
	}
	if opts.NoGrants {
		grants = nil
	}
	// Every subject the suite presents needs a published principal: a
	// credential that resolves to nobody in the snapshot is refused, which is
	// the behaviour ADR 0019 wants and which tests must therefore satisfy
	// rather than work around.
	principals := make([]*snapshotpb.Principal, 0, len(testSubjects))
	for _, subject := range testSubjects {
		principals = append(principals, &snapshotpb.Principal{
			Id: subject, TenantId: "tn_acme", Subject: subject, Grants: grants,
		})
	}
	snap, err := b.Build()
	require.NoError(h.t, err)
	signed, err := h.Signer.Sign(snap)
	require.NoError(h.t, err)
	_, err = h.Store.Activate(signed, h.Verifier)
	require.NoError(h.t, err)

	// After the snapshot, because a principal names a tenant the snapshot has
	// to carry — which is also the order the data plane loads them in.
	applyPrincipals(h.Store, authz.DefaultCatalog(), principals)
}

// Connect opens a real MCP client session against the gateway.
func (h *harness) Connect(t *testing.T, headers http.Header) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{
		Name: "conformance-client", Version: "1.0.0",
	}, &sdk.ClientOptions{
		// The gateway's MRTR behaviour is under test, so the client must not
		// silently fulfil input requests on our behalf.
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})

	httpClient := &http.Client{Timeout: 20 * time.Second}
	if len(headers) > 0 {
		httpClient = withHeaders(httpClient, headers)
	}

	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:             h.URL(),
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// URL is the gateway's endpoint for an audience.
// URL is the single MCP endpoint. There is no audience segment: the principal
// comes from the credential (ADR 0019).
func (h *harness) URL() string {
	return h.Server.URL + "/mcp"
}

// rawOrNil recovers the raw JSON schema the adapter produced.
func rawOrNil(v any) json.RawMessage {
	switch s := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return s
	case []byte:
		return s
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return raw
	}
}

type headerTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	for k, values := range t.header {
		for _, v := range values {
			out.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(out)
}

func withHeaders(client *http.Client, header http.Header) *http.Client {
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	clone := *client
	clone.Transport = &headerTransport{base: base, header: header.Clone()}
	return &clone
}

// testPrincipalID is the identity every harness connection resolves to.
//
// One principal for the whole harness because these tests are about the
// protocol and the pipeline, not about authorization — the grant tests live in
// internal/platform/authz and internal/dataplane/snapshot.
const testPrincipalID = "dev-user"

// testSubjects are every identity the suite presents. The dev resolver uses the
// subject as the principal id, so each needs a published principal.
var testSubjects = []string{
	testPrincipalID,
	"alice@example.com",
	"bob@example.com",
	"admin@example.com",
	"intern@example.com",
}

// applyPrincipals installs a principal set on a store.
//
// Principals live in their own signed artifact now (ADR 0024), so a test that
// wants a catalog composes one from two things rather than from a builder call.
// Indexed without a signature, which is what [snapshot.IndexPrincipals] exists
// for.
func applyPrincipals(store *snapshot.Store, catalog authz.Catalog, principals []*snapshotpb.Principal) {
	// One past whatever the store holds. A fixed version would make a second
	// publish in one test fail the monotonicity check, which is the store doing
	// its job — the helper just has to respect it.
	set := &snapshotpb.PrincipalSet{
		Version:    store.Principals().Version + 1,
		Principals: principals,
	}
	for _, role := range catalog.Roles() {
		for _, p := range catalog.Permissions(role) {
			set.RolePermissions = append(set.RolePermissions,
				&snapshotpb.RolePermission{Role: role, Permission: string(p)})
		}
	}
	indexed, err := snapshot.IndexPrincipals(set)
	if err != nil {
		panic("test principal set is malformed: " + err.Error())
	}
	if err := store.ApplyPrincipals(indexed); err != nil {
		panic("applying the test principal set: " + err.Error())
	}
}
