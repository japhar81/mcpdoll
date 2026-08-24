// Copyright 2026 Henry Zektser.

package wiring_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/fixtures"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/plugins"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/wiring"
	mcpadapter "github.com/mcpdoll/mcpdoll/internal/mcp"
	"github.com/mcpdoll/mcpdoll/internal/platform/authz"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// This is the whole data plane, end to end, with a real WASM plugin in the path:
//
//	MCP client -> edge -> pipeline -> wazero -> compiled plugin
//	           -> RFC 6902 patch (scope-checked) -> backend -> client
//
// Every layer is the real one. The point is that the *composition* is what breaks
// — each package's own tests pass while the payload shape they agree on is
// subtly different — and only running the whole thing catches that.

// ---------------------------------------------------------- plugin building --

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

func pluginPath(t *testing.T, name string) (path, digest string) {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mcpdoll-wiring-*")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		root, err := filepath.Abs(filepath.Join("..", "..", ".."))
		if err != nil {
			buildErr = err
			return
		}
		for _, plugin := range []string{"redact", "entitlements"} {
			cmd := exec.Command("go", "build", "-buildmode=c-shared",
				"-o", filepath.Join(dir, plugin+".wasm"), "./plugins/"+plugin)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("building %s: %w\n%s", plugin, err, out)
				return
			}
		}
	})
	require.NoError(t, buildErr)

	path = filepath.Join(buildDir, name+".wasm")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return "file://" + path, "sha256:" + hex.EncodeToString(sum[:])
}

// ------------------------------------------------------------------ harness --

type harness struct {
	t        *testing.T
	backend  *fixtures.Backend
	store    *snapshot.Store
	signer   *snapshot.Signer
	verifier *snapshot.Verifier
	server   *http.Server
	url      string
	version  int64
}

func newHarness(t *testing.T, manifests ...*snapshotpb.PluginManifest) *harness {
	t.Helper()

	h := &harness{t: t, backend: fixtures.NewModern()}
	h.backend.Start()
	t.Cleanup(h.backend.Close)

	pub, priv, err := snapshot.GenerateKey()
	require.NoError(t, err)
	h.signer, err = snapshot.NewSigner("test", priv)
	require.NoError(t, err)
	h.verifier, err = snapshot.NewVerifierFromKeys(map[string]ed25519.PublicKey{"test": pub})
	require.NoError(t, err)

	h.store = snapshot.NewStore(3)

	pool := backends.New(backends.Options{DialTimeout: 5 * time.Second})
	t.Cleanup(pool.Close)

	wasmHost, err := plugins.NewWASMHost(context.Background(), plugins.WASMOptions{})
	require.NoError(t, err)
	registry := wiring.NewHostRegistry(wasmHost, nil)
	t.Cleanup(func() { _ = registry.Close() })

	engine, err := pipeline.New(pipeline.Options{
		Hosts: registry,
		// Generous, because compiling and instantiating a Go-built WASM module
		// costs far more than any plugin's actual work. Budget enforcement is
		// tested in the pipeline package with a scripted host.
		TotalBudget:  30 * time.Second,
		HookBudget:   15 * time.Second,
		PluginBudget: 15 * time.Second,
	})
	require.NoError(t, err)

	// Plugins load on activation, before the edge rebuilds.
	h.store.Observe(func(view *snapshot.View) { registry.Sync(context.Background(), view) })

	identity, err := edge.NewHeaderIdentityResolver("test", "dev-user", []string{"support"})
	require.NoError(t, err)

	dp, err := edge.New(edge.Options{
		Store:    h.store,
		Pool:     pool,
		Identity: identity,
		Pipeline: wiring.NewEdgePipeline(engine, nil),
	})
	require.NoError(t, err)

	listener := newListener(t)
	h.url = "http://" + listener.Addr().String()
	h.server = &http.Server{Handler: dp.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = h.server.Serve(listener) }()
	t.Cleanup(func() { _ = h.server.Close() })

	h.publish(manifests...)
	return h
}

func (h *harness) publish(manifests ...*snapshotpb.PluginManifest) {
	h.t.Helper()
	h.version++

	b := snapshot.NewBuilder(h.version).
		WithCatalogDefaults(5*time.Minute, 30*time.Second)
	b.AddTenant(&snapshotpb.Tenant{Id: "tn_test", Slug: "test", Name: "Test", Status: "active"})
	b.AddToolset(&snapshotpb.Toolset{Id: "ts_test", Name: "test", Priority: 10})
	// Every subject these tests present needs a published principal: a
	// credential resolving to nobody in the snapshot is refused (ADR 0019).
	var principals []*snapshotpb.Principal
	for _, subject := range []string{"dev-user", "alice@example.com", "admin@example.com"} {
		principals = append(principals, &snapshotpb.Principal{
			Id: subject, TenantId: "tn_test", Subject: subject,
			Grants: []*snapshotpb.Grant{
				{Role: authz.RoleToolUser, Scope: authz.TenantScope("test")},
			},
		})
	}
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_crm", Name: "crm", Prefix: "crm"})
	b.AddServer(&snapshotpb.Server{
		Id: "srv_crm", Name: "crm-prod", NamespaceId: "ns_crm", Bindings: []*snapshotpb.Binding{{TenantId: "tn_test", Primary: h.backend.URL()}},
	})

	discovered, err := mcpadapter.Discover(context.Background(), mcpadapter.DiscoverOptions{
		Endpoint: h.backend.URL(), Timeout: 10 * time.Second,
	})
	require.NoError(h.t, err)
	for _, tool := range discovered.Tools {
		def, err := mcpadapter.ToCanonical(tool)
		require.NoError(h.t, err)
		effect := snapshotpb.EffectClass_EFFECT_CLASS_READ
		if tool.Name == "update_customer" {
			effect = snapshotpb.EffectClass_EFFECT_CLASS_WRITE
		}
		b.AddTool(snapshot.ToolInput{
			ServerID: "srv_crm", NamespaceID: "ns_crm",
			TenantID: "tn_test", ToolsetID: "ts_test", Prefix: "crm",
			Name: def.Name, Title: def.Title, Description: def.Description,
			InputSchema: rawJSON(def.InputSchema),
			EffectClass: effect,
		})
	}

	for _, m := range manifests {
		b.AddPlugin(m)
	}

	snap, err := b.Build()
	require.NoError(h.t, err)
	signed, err := h.signer.Sign(snap)
	require.NoError(h.t, err)
	_, err = h.store.Activate(signed, h.verifier)
	require.NoError(h.t, err)
	applyPrincipals(h.store, authz.DefaultCatalog(), principals)
}

func (h *harness) connect(t *testing.T, subject string, groups ...string) *sdk.ClientSession {
	t.Helper()
	header := http.Header{}
	if subject != "" {
		header.Set(edge.HeaderSubject, subject)
	}
	if len(groups) > 0 {
		header.Set(edge.HeaderGroups, strings.Join(groups, ","))
	}

	client := sdk.NewClient(&sdk.Implementation{Name: "wiring-test", Version: "1"},
		&sdk.ClientOptions{MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true}})
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint:             h.url + "/mcp",
		HTTPClient:           &http.Client{Timeout: 60 * time.Second, Transport: headerRT{header}},
		DisableStandaloneSSE: true,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// ---------------------------------------------------------------- the tests --

// TestRedactPluginChangesALiveResponse is the composition proof: a real WASM
// plugin, invoked by the real engine, patching a real backend's result before it
// reaches a real MCP client.
func TestRedactPluginChangesALiveResponse(t *testing.T) {
	path, digest := pluginPath(t, "redact")
	h := newHarness(t, &snapshotpb.PluginManifest{
		Id: "plg_redact", Name: "redact", Version: "1.0.0",
		Runtime:     snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:       []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_RESULT},
		Writes:      []string{"result.content"},
		Rollout:     snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,
		ArtifactRef: path, ArtifactDigest: digest,
	})

	session := h.connect(t, "alice@example.com")
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "crm.get_payment_method", Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, text(res))

	got := text(res)
	require.NotContains(t, got, "4111 1111 1111 1111",
		"the card number must not reach the client")
	require.Contains(t, got, "[REDACTED]")
	// Everything else survives: a redaction plugin that mangles the response is
	// one an operator disables.
	require.Contains(t, got, "customer cus_1")
	require.Contains(t, got, "expires 04/29")
	require.Contains(t, got, "billing zip 94107")
}

// TestNoPluginMeansNoChange: the same call without the plugin returns the card
// number, so the previous test is measuring the plugin and not the fixture.
func TestNoPluginMeansNoChange(t *testing.T) {
	h := newHarness(t)
	session := h.connect(t, "alice@example.com")

	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "crm.get_payment_method", Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.Contains(t, text(res), "4111 1111 1111 1111",
		"without the plugin the backend's response passes through unchanged")
}

// TestShadowPluginDoesNotChangeTheResponse: shadow means recorded, not acted on.
func TestShadowPluginDoesNotChangeTheResponse(t *testing.T) {
	path, digest := pluginPath(t, "redact")
	h := newHarness(t, &snapshotpb.PluginManifest{
		Id: "plg_redact", Name: "redact", Version: "1.0.0",
		Runtime:     snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:       []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_RESULT},
		Writes:      []string{"result.content"},
		Rollout:     snapshotpb.RolloutState_ROLLOUT_STATE_SHADOW,
		ArtifactRef: path, ArtifactDigest: digest,
	})

	session := h.connect(t, "alice@example.com")
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "crm.get_payment_method", Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.Contains(t, text(res), "4111 1111 1111 1111",
		"a shadow plugin must not change what the client receives")
}

// TestEntitlementsFiltersTheCatalogAndRefusesTheCall covers the pairing that
// makes entitlements real: hiding a tool is cosmetic on its own, because a model
// that has seen the name once can still call it.
func TestEntitlementsFiltersTheCatalogAndRefusesTheCall(t *testing.T) {
	path, digest := pluginPath(t, "entitlements")
	config, err := json.Marshal(map[string]any{
		"default":     "allow",
		"hide_denied": true,
		"rules": []map[string]any{
			{"match": "crm.update_customer", "allow_groups": []string{"crm-admins"}},
		},
	})
	require.NoError(t, err)

	h := newHarness(t, &snapshotpb.PluginManifest{
		Id: "plg_ent", Name: "entitlements", Version: "1.0.0",
		Runtime: snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks: []snapshotpb.Hook{
			snapshotpb.Hook_HOOK_ON_CATALOG,
			snapshotpb.Hook_HOOK_ON_TOOL_CALL,
		},
		Writes:            []string{"catalog"},
		IdentityDependent: true,
		Rollout:           snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,
		ArtifactRef:       path,
		ArtifactDigest:    digest,
		ConfigJson:        string(config),
	})

	ctx := context.Background()

	t.Run("an unentitled principal", func(t *testing.T) {
		session := h.connect(t, "alice@example.com", "support")

		list, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		require.NotContains(t, names(list.Tools), "crm.update_customer",
			"the tool should be hidden from the catalog")
		require.Contains(t, names(list.Tools), "crm.lookup_customer",
			"and everything else should still be there")

		require.Equal(t, "private", list.CacheScope,
			"a catalog an identity-dependent plugin filtered must not be shareable")

		// Naming the hidden tool anyway is refused, not served.
		res, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name: "crm.update_customer", Arguments: map[string]any{"customer_id": "cus_1"},
		})
		require.NoError(t, err)
		require.True(t, res.IsError,
			"hiding a tool is cosmetic unless the call is refused too")
		require.Contains(t, text(res), "entitlement")
		require.Zero(t, h.backend.Calls("update_customer"),
			"the refused call must not have reached the backend")
	})

	t.Run("an entitled principal", func(t *testing.T) {
		session := h.connect(t, "admin@example.com", "support", "crm-admins")

		list, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		require.Contains(t, names(list.Tools), "crm.update_customer")

		res, err := session.CallTool(ctx, &sdk.CallToolParams{
			Name: "crm.update_customer", Arguments: map[string]any{"customer_id": "cus_1"},
		})
		require.NoError(t, err)
		require.False(t, res.IsError, text(res))
		require.Equal(t, 1, h.backend.Calls("update_customer"))
	})
}

// TestPluginCannotAddToTheCatalog: a plugin may remove and reorder, never invent.
// What the gateway serves is the admitted definition (ADR 0006), and a plugin is
// not an admission decision.
func TestPluginCannotAddToTheCatalog(t *testing.T) {
	path, digest := pluginPath(t, "entitlements")
	h := newHarness(t, &snapshotpb.PluginManifest{
		Id: "plg_ent", Name: "entitlements", Version: "1.0.0",
		Runtime:        snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:          []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_CATALOG},
		Writes:         []string{"catalog"},
		Rollout:        snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,
		ArtifactRef:    path,
		ArtifactDigest: digest,
	})

	session := h.connect(t, "alice@example.com")
	list, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)

	// Every served name must correspond to an admitted tool.
	for _, name := range names(list.Tools) {
		require.True(t, strings.HasPrefix(name, "crm."),
			"a plugin must not be able to introduce a name %q", name)
	}
}

// TestUnloadablePluginDoesNotBreakTheGateway: a plugin whose artifact is missing
// is skipped and recorded, not fatal. One broken plugin must not block a
// configuration change that has nothing to do with it.
func TestUnloadablePluginDoesNotBreakTheGateway(t *testing.T) {
	h := newHarness(t, &snapshotpb.PluginManifest{
		Id: "plg_missing", Name: "missing", Version: "1.0.0",
		Runtime:        snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:          []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_RESULT},
		Rollout:        snapshotpb.RolloutState_ROLLOUT_STATE_ENFORCE,
		ArtifactRef:    "file:///nonexistent/plugin.wasm",
		ArtifactDigest: "sha256:" + strings.Repeat("0", 64),
	})

	session := h.connect(t, "alice@example.com")
	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "crm.lookup_customer", Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, text(res))
	require.Contains(t, text(res), "Acme Corp",
		"a plugin that could not load must not take the request with it")
}

// TestGRPCPluginIsRefusedNotIgnored: the gRPC runtime is not built. A configured
// security control that silently does nothing is worse than one that says so.
func TestGRPCPluginIsRefusedNotIgnored(t *testing.T) {
	wasmHost, err := plugins.NewWASMHost(context.Background(), plugins.WASMOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = wasmHost.Close() })

	registry := wiring.NewHostRegistry(wasmHost, nil)
	_, err = registry.Host(&snapshotpb.PluginManifest{
		Id: "plg_grpc", Name: "guard",
		Runtime: snapshotpb.PluginRuntime_PLUGIN_RUNTIME_GRPC,
	})
	require.Error(t, err)
}

// TestHostRegistryReportsLoadFailures gives the console something to show.
func TestHostRegistryReportsLoadFailures(t *testing.T) {
	path, digest := pluginPath(t, "redact")
	h := newHarness(t)
	_ = h

	wasmHost, err := plugins.NewWASMHost(context.Background(), plugins.WASMOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = wasmHost.Close() })
	registry := wiring.NewHostRegistry(wasmHost, nil)

	good := &snapshotpb.PluginManifest{
		Id: "plg_ok", Name: "redact",
		Runtime:     snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:       []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_RESULT},
		ArtifactRef: path, ArtifactDigest: digest,
	}
	bad := &snapshotpb.PluginManifest{
		Id: "plg_bad", Name: "wrong-digest",
		Runtime:        snapshotpb.PluginRuntime_PLUGIN_RUNTIME_WASM,
		Hooks:          []snapshotpb.Hook{snapshotpb.Hook_HOOK_ON_TOOL_RESULT},
		ArtifactRef:    path,
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
	}

	view := buildView(t, good, bad)
	registry.Sync(context.Background(), view)

	require.Equal(t, []string{"plg_ok"}, registry.Loaded())
	failures := registry.Failures()
	require.Contains(t, failures, "plg_bad")
	require.Contains(t, failures["plg_bad"], "digest mismatch")
}

// ---------------------------------------------------------------- helpers ----

func buildView(t *testing.T, manifests ...*snapshotpb.PluginManifest) *snapshot.View {
	t.Helper()
	b := snapshot.NewBuilder(1).WithCatalogDefaults(time.Minute, time.Second)
	b.AddTenant(&snapshotpb.Tenant{Id: "tn_test", Slug: "test", Name: "Test", Status: "active"})
	b.AddToolset(&snapshotpb.Toolset{Id: "ts_test", Name: "test", Priority: 10})
	b.AddNamespace(&snapshotpb.Namespace{Id: "ns_a", Name: "a", Prefix: "a"})
	b.AddServer(&snapshotpb.Server{Id: "srv_a", NamespaceId: "ns_a", Bindings: []*snapshotpb.Binding{{TenantId: "tn_test", Primary: "http://localhost:1"}}})
	b.AddTool(snapshot.ToolInput{
		ServerID: "srv_a", NamespaceID: "ns_a",
		TenantID: "tn_test", ToolsetID: "ts_test", Prefix: "a", Name: "t",
		InputSchema: []byte(`{"type":"object"}`),
		EffectClass: snapshotpb.EffectClass_EFFECT_CLASS_READ,
	})
	for _, m := range manifests {
		b.AddPlugin(m)
	}
	snap, err := b.Build()
	require.NoError(t, err)
	view, err := snapshot.Build(snap)
	require.NoError(t, err)
	return view
}

func text(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func names(tools []*sdk.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

func rawJSON(v any) []byte {
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

type headerRT struct{ header http.Header }

func (t headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	for k, values := range t.header {
		for _, v := range values {
			out.Header.Set(k, v)
		}
	}
	return http.DefaultTransport.RoundTrip(out)
}

// newListener binds an ephemeral port. httptest.Server is avoided here because
// the harness needs the URL before the handler is fully wired.
func newListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l
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
