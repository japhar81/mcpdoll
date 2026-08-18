// Copyright 2026 The MCPDoll Authors.

package health

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/mcp"
	"github.com/mcpdoll/mcpdoll/internal/platform/canonical"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// The dual-digest scheme exists so that drift can be classified rather than
// merely detected. These tests are about that distinction: a reworded
// description and a changed schema are both "the digest differs", and treating
// them the same would either churn every client's prompt cache over a typo fix
// or serve a tool whose arguments no longer mean what they did.

func admittedTool(t *testing.T, def *canonical.ToolDefinition, qualified string) *snapshotpb.ToolDefinition {
	t.Helper()

	full, err := def.Digest()
	require.NoError(t, err)
	semantic, err := def.SemanticDigest()
	require.NoError(t, err)

	out := &snapshotpb.ToolDefinition{
		Digest:         string(full),
		SemanticDigest: string(semantic),
		Name:           def.Name,
		QualifiedName:  qualified,
		Title:          def.Title,
		Description:    def.Description,
	}
	if def.InputSchema != nil {
		raw, err := canonical.Marshal(def.InputSchema)
		require.NoError(t, err)
		out.InputSchemaJson = string(raw)
	}
	if len(def.Annotations) > 0 {
		raw, err := canonical.Marshal(def.Annotations)
		require.NoError(t, err)
		out.AnnotationsJson = string(raw)
	}
	return out
}

func schema(t *testing.T, raw string) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(raw), &v))
	return v
}

func digestsOf(t *testing.T, defs ...*canonical.ToolDefinition) map[string]mcp.ToolDigests {
	t.Helper()
	out := map[string]mcp.ToolDigests{}
	for _, def := range defs {
		full, err := def.Digest()
		require.NoError(t, err)
		semantic, err := def.SemanticDigest()
		require.NoError(t, err)
		out[def.Name] = mcp.ToolDigests{Full: full, Semantic: semantic, Definition: def}
	}
	return out
}

func baseDef(t *testing.T) *canonical.ToolDefinition {
	t.Helper()
	return &canonical.ToolDefinition{
		Name:        "check_stock",
		Description: "Report how many units of a SKU are on hand.",
		InputSchema: schema(t, `{"type":"object","properties":{"sku":{"type":"string"}}}`),
	}
}

func TestAnUnchangedToolProducesNoDrift(t *testing.T) {
	t.Parallel()
	def := baseDef(t)
	admitted := map[string]*snapshotpb.ToolDefinition{
		"check_stock": admittedTool(t, def, "whs.check_stock"),
	}

	require.Empty(t, Diff(admitted, digestsOf(t, def)))
}

func TestARewordedDescriptionIsCosmetic(t *testing.T) {
	t.Parallel()
	admitted := map[string]*snapshotpb.ToolDefinition{
		"check_stock": admittedTool(t, baseDef(t), "whs.check_stock"),
	}

	reworded := baseDef(t)
	reworded.Description = "Reports on-hand units for a SKU. Now with 40% more prose!"

	drifts := Diff(admitted, digestsOf(t, reworded))
	require.Len(t, drifts, 1)
	require.Equal(t, DriftCosmetic, drifts[0].Kind)
	require.Contains(t, drifts[0].Detail, "description")
	require.Contains(t, drifts[0].Detail, "schema is unchanged")

	// The whole point of the semantic digest: this must not stop the tool being
	// served, even under strict mode.
	require.False(t, drifts[0].Kind.Blocking())
}

func TestAChangedSchemaIsSemantic(t *testing.T) {
	t.Parallel()
	admitted := map[string]*snapshotpb.ToolDefinition{
		"check_stock": admittedTool(t, baseDef(t), "whs.check_stock"),
	}

	changed := baseDef(t)
	changed.InputSchema = schema(t,
		`{"type":"object","properties":{"sku":{"type":"string"},"warehouse":{"type":"string"}},"required":["warehouse"]}`)

	drifts := Diff(admitted, digestsOf(t, changed))
	require.Len(t, drifts, 1)
	require.Equal(t, DriftSemantic, drifts[0].Kind)
	require.Equal(t, "input schema changed", drifts[0].Detail)
	require.True(t, drifts[0].Kind.Blocking())
}

func TestReorderingASchemaIsNotDrift(t *testing.T) {
	t.Parallel()
	admitted := map[string]*snapshotpb.ToolDefinition{
		"check_stock": admittedTool(t, baseDef(t), "whs.check_stock"),
	}

	// Same document, keys in a different order. Canonicalization exists exactly
	// so a backend that re-serializes its schema does not read as a change.
	reordered := baseDef(t)
	reordered.InputSchema = schema(t, `{"properties":{"sku":{"type":"string"}},"type":"object"}`)

	require.Empty(t, Diff(admitted, digestsOf(t, reordered)))
}

func TestAChangedAnnotationIsSemanticNotCosmetic(t *testing.T) {
	t.Parallel()
	def := baseDef(t)
	def.Annotations = map[string]any{"readOnlyHint": true}
	admitted := map[string]*snapshotpb.ToolDefinition{
		"check_stock": admittedTool(t, def, "whs.check_stock"),
	}

	// A tool that stops claiming to be read-only has changed what it may do.
	// Classifying that as prose because it is not the schema would be the
	// dangerous mistake this scheme exists to prevent.
	flipped := baseDef(t)
	flipped.Annotations = map[string]any{"readOnlyHint": false}

	drifts := Diff(admitted, digestsOf(t, flipped))
	require.Len(t, drifts, 1)
	require.Equal(t, DriftSemantic, drifts[0].Kind)
	require.Equal(t, "annotations changed", drifts[0].Detail)
}

func TestAWithdrawnToolIsRemoved(t *testing.T) {
	t.Parallel()
	admitted := map[string]*snapshotpb.ToolDefinition{
		"check_stock": admittedTool(t, baseDef(t), "whs.check_stock"),
	}

	drifts := Diff(admitted, map[string]mcp.ToolDigests{})
	require.Len(t, drifts, 1)
	require.Equal(t, DriftRemoved, drifts[0].Kind)
	require.Equal(t, "whs.check_stock", drifts[0].QualifiedName)
	require.True(t, drifts[0].Kind.Blocking())
}

func TestAnUnadmittedToolIsReportedButNeverNamed(t *testing.T) {
	t.Parallel()
	extra := &canonical.ToolDefinition{
		Name:        "delete_warehouse",
		Description: "Irreversibly removes a warehouse.",
		InputSchema: schema(t, `{"type":"object"}`),
	}

	drifts := Diff(map[string]*snapshotpb.ToolDefinition{}, digestsOf(t, extra))
	require.Len(t, drifts, 1)
	require.Equal(t, DriftAdded, drifts[0].Kind)

	// No qualified name. Assigning one here would be exactly the runtime
	// auto-naming the design forbids — the prefix is admission's to grant.
	require.Empty(t, drifts[0].QualifiedName)
	require.False(t, drifts[0].Kind.Blocking())
}

func TestDriftIsOrderedBySeverityThenName(t *testing.T) {
	t.Parallel()

	gone := baseDef(t)
	gone.Name = "zzz_gone"
	reworded := baseDef(t)
	reworded.Name = "aaa_reworded"
	rescheduled := baseDef(t)
	rescheduled.Name = "mmm_semantic"

	admitted := map[string]*snapshotpb.ToolDefinition{
		"zzz_gone":     admittedTool(t, gone, "whs.zzz_gone"),
		"aaa_reworded": admittedTool(t, reworded, "whs.aaa_reworded"),
		"mmm_semantic": admittedTool(t, rescheduled, "whs.mmm_semantic"),
	}

	rewordedNow := baseDef(t)
	rewordedNow.Name = "aaa_reworded"
	rewordedNow.Description = "different words"

	semanticNow := baseDef(t)
	semanticNow.Name = "mmm_semantic"
	semanticNow.InputSchema = schema(t, `{"type":"object","properties":{"other":{"type":"number"}}}`)

	added := baseDef(t)
	added.Name = "bbb_added"

	drifts := Diff(admitted, digestsOf(t, rewordedNow, semanticNow, added))
	kinds := make([]DriftKind, len(drifts))
	for i, d := range drifts {
		kinds[i] = d.Kind
	}
	// Removed first — a tool that is gone will certainly fail, where a
	// semantically drifted one may still work. Stable order matters because an
	// alerting rule that re-fires on a reshuffled list is useless.
	require.Equal(t,
		[]DriftKind{DriftRemoved, DriftSemantic, DriftCosmetic, DriftAdded}, kinds)
}
