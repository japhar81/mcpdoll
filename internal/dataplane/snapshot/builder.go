// Copyright 2026 The MCPDoll Authors.

package snapshot

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/mcpdoll/mcpdoll/internal/platform/canonical"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// Builder assembles a Snapshot message.
//
// It lives beside the verifier rather than in the control plane so that the
// same construction and validation logic backs the control plane's snapshotter,
// the test fixtures, and the CLI's local `snapshot build`. One implementation
// means a snapshot a test builds is structurally the same artifact production
// builds.
type Builder struct {
	snap *snapshotpb.Snapshot
	errs []string
}

// NewBuilder starts a snapshot for an organization at a version.
func NewBuilder(orgID string, version int64) *Builder {
	return &Builder{
		snap: &snapshotpb.Snapshot{
			Version: version,
			OrgId:   orgID,
			BuiltAt: timestamppb.New(time.Now().UTC()),
			Catalog: &snapshotpb.CatalogDefaults{
				TtlMs:         int32((5 * time.Minute).Milliseconds()),
				DegradedTtlMs: int32((30 * time.Second).Milliseconds()),
			},
		},
	}
}

// WithID sets the build identifier.
func (b *Builder) WithID(id string) *Builder {
	b.snap.Id = id
	return b
}

// WithRegistryDigest records the registry state this was resolved from.
func (b *Builder) WithRegistryDigest(d string) *Builder {
	b.snap.RegistryDigest = d
	return b
}

// WithCatalogDefaults sets the org-wide TTLs.
func (b *Builder) WithCatalogDefaults(ttl, degraded time.Duration) *Builder {
	if degraded > ttl {
		b.errs = append(b.errs, fmt.Sprintf(
			"catalog degraded TTL (%s) exceeds the normal TTL (%s): the degraded value must be shorter so clients re-ask sooner",
			degraded, ttl))
	}
	b.snap.Catalog = &snapshotpb.CatalogDefaults{
		TtlMs:         int32(ttl.Milliseconds()),
		DegradedTtlMs: int32(degraded.Milliseconds()),
	}
	return b
}

// AddNamespace registers a namespace and its immutable prefix.
func (b *Builder) AddNamespace(ns *snapshotpb.Namespace) *Builder {
	if ns.Prefix == "" {
		b.errs = append(b.errs, fmt.Sprintf("namespace %q has no prefix", ns.Id))
	}
	if strings.Contains(ns.Prefix, ".") {
		// A dot in the prefix would make the qualified name ambiguous: the
		// gateway splits on the first dot to route.
		b.errs = append(b.errs, fmt.Sprintf(
			"namespace %q prefix %q contains a dot, which would make qualified names ambiguous",
			ns.Id, ns.Prefix))
	}
	b.snap.Namespaces = append(b.snap.Namespaces, ns)
	return b
}

func (b *Builder) AddServer(s *snapshotpb.Server) *Builder {
	b.snap.Servers = append(b.snap.Servers, s)
	return b
}

func (b *Builder) AddBundle(bundle *snapshotpb.Bundle) *Builder {
	b.snap.Bundles = append(b.snap.Bundles, bundle)
	return b
}

func (b *Builder) AddAudience(a *snapshotpb.Audience) *Builder {
	b.snap.Audiences = append(b.snap.Audiences, a)
	return b
}

func (b *Builder) AddPolicy(p *snapshotpb.Policy) *Builder {
	b.snap.Policies = append(b.snap.Policies, p)
	return b
}

func (b *Builder) AddPlugin(p *snapshotpb.PluginManifest) *Builder {
	b.snap.Plugins = append(b.snap.Plugins, p)
	return b
}

// ToolInput is a tool as callers hand it to the builder, before the builder
// computes the derived fields that make it a snapshot entry.
type ToolInput struct {
	ServerID     string
	NamespaceID  string
	Prefix       string
	Name         string
	Title        string
	Description  string
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  map[string]any
	EffectClass  snapshotpb.EffectClass
}

// AddTool canonicalizes a definition, computes its digests and derived fields,
// and appends it.
//
// The digests, the qualified name, and the idempotency requirement are all
// computed here rather than supplied by the caller. Every one of them is a
// function of the definition, and letting a caller pass its own value is how
// they end up disagreeing with the content they claim to describe.
func (b *Builder) AddTool(in ToolInput) *Builder {
	if in.Name == "" {
		b.errs = append(b.errs, "tool has no name")
		return b
	}
	if in.Prefix == "" {
		b.errs = append(b.errs, fmt.Sprintf("tool %q has no namespace prefix", in.Name))
		return b
	}

	def := &canonical.ToolDefinition{
		Name:        in.Name,
		Title:       in.Title,
		Description: in.Description,
		Annotations: in.Annotations,
	}
	if len(in.InputSchema) > 0 {
		def.InputSchema = in.InputSchema
	}
	if len(in.OutputSchema) > 0 {
		def.OutputSchema = in.OutputSchema
	}

	digest, err := def.Digest()
	if err != nil {
		b.errs = append(b.errs, fmt.Sprintf("tool %q: %v", in.Name, err))
		return b
	}
	semantic, err := def.SemanticDigest()
	if err != nil {
		b.errs = append(b.errs, fmt.Sprintf("tool %q: %v", in.Name, err))
		return b
	}

	inputJSON, err := canonicalizeOrEmpty(in.InputSchema)
	if err != nil {
		b.errs = append(b.errs, fmt.Sprintf("tool %q input schema: %v", in.Name, err))
		return b
	}
	outputJSON, err := canonicalizeOrEmpty(in.OutputSchema)
	if err != nil {
		b.errs = append(b.errs, fmt.Sprintf("tool %q output schema: %v", in.Name, err))
		return b
	}
	var annotationsJSON string
	if len(in.Annotations) > 0 {
		raw, err := canonical.Marshal(in.Annotations)
		if err != nil {
			b.errs = append(b.errs, fmt.Sprintf("tool %q annotations: %v", in.Name, err))
			return b
		}
		annotationsJSON = string(raw)
	}

	canonicalForm, err := def.CanonicalForm()
	if err != nil {
		b.errs = append(b.errs, fmt.Sprintf("tool %q: %v", in.Name, err))
		return b
	}

	b.snap.Tools = append(b.snap.Tools, &snapshotpb.ToolDefinition{
		Digest:         digest.String(),
		SemanticDigest: semantic.String(),
		ServerId:       in.ServerID,
		NamespaceId:    in.NamespaceID,
		Name:           in.Name,
		QualifiedName:  in.Prefix + "." + in.Name,
		Title:          in.Title,
		Description:    in.Description,

		InputSchemaJson:  inputJSON,
		OutputSchemaJson: outputJSON,
		AnnotationsJson:  annotationsJSON,

		EffectClass:   in.EffectClass,
		TokenEstimate: int32(EstimateTokens(canonicalForm)),
		RequiresIdempotencyKey: in.EffectClass == snapshotpb.EffectClass_EFFECT_CLASS_WRITE ||
			in.EffectClass == snapshotpb.EffectClass_EFFECT_CLASS_DESTRUCTIVE,
	})
	return b
}

func canonicalizeOrEmpty(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	out, err := canonical.CanonicalizeSchema(raw)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// Build validates and returns the assembled snapshot.
func (b *Builder) Build() (*snapshotpb.Snapshot, error) {
	if len(b.errs) > 0 {
		return nil, fmt.Errorf("snapshot: %d build problem(s):\n  - %s",
			len(b.errs), strings.Join(b.errs, "\n  - "))
	}
	// Round-trip through the view builder so a snapshot that cannot be served
	// is never signed. Catching a dangling reference here makes it a build
	// failure with a name in it, rather than a refused activation on every
	// data-plane instance simultaneously.
	if _, err := Build(b.snap); err != nil {
		return nil, err
	}
	return b.snap, nil
}

// EstimateTokens approximates how many tokens a serialized definition costs the
// model that has to read it.
//
// This is a heuristic, and it is used for budgets and the console's meter, not
// for billing. Four bytes per token is the usual rule of thumb for English
// prose; JSON is punctuation-dense and tokenizes worse, so structural characters
// are counted more heavily. Getting this exactly right would mean shipping a
// tokenizer per model family and pinning its version, which buys precision the
// budget check does not need.
func EstimateTokens(canonicalForm []byte) int {
	if len(canonicalForm) == 0 {
		return 0
	}
	var structural int
	for _, c := range canonicalForm {
		switch c {
		case '{', '}', '[', ']', ':', ',', '"':
			structural++
		}
	}
	prose := len(canonicalForm) - structural
	// Structural characters tokenize roughly one-to-one; prose at ~4 bytes per
	// token. Round up so a budget is never under-reported.
	return structural + (prose+3)/4
}
