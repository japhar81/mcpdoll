// Copyright 2026 The MCPDoll Authors.

// Package health probes backends, classifies what they publish against what was
// admitted, and tracks whether each one is fit to serve.
//
// The premise is ADR 0006: the gateway serves admitted definitions from the
// snapshot, never live backend output. That makes a backend changing its
// catalog a *detectable event* rather than a silent change to what clients see
// — but only if something looks. This is the thing that looks.
package health

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mcpdoll/mcpdoll/internal/mcp"
	"github.com/mcpdoll/mcpdoll/internal/platform/canonical"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// DriftKind classifies one difference between admitted and observed.
type DriftKind string

const (
	// DriftCosmetic is a changed description or title: the digest differs, the
	// semantic digest does not. Safe to keep serving — the admitted prose is
	// what clients already have, and swapping it would churn prompt caches for
	// a reworded sentence.
	DriftCosmetic DriftKind = "cosmetic"

	// DriftSemantic is a changed schema, or a changed annotation. The tool no
	// longer does what was admitted, and calling it with admitted arguments may
	// mean something different than it did. This is the one that matters.
	DriftSemantic DriftKind = "semantic"

	// DriftRemoved is an admitted tool the backend no longer publishes. Calls
	// will fail; the catalog is not changed, because dropping the tool would
	// invalidate every client's prompt cache over a backend deploy that may be
	// mid-rollout.
	DriftRemoved DriftKind = "removed"

	// DriftAdded is a tool the backend publishes that was never admitted. It is
	// *not* served — that is the whole point of admission — and is reported so
	// somebody publishes it deliberately.
	DriftAdded DriftKind = "added"
)

// Severity orders the kinds for display and for deciding what to act on.
//
// Removed outranks semantic: a tool that is gone will certainly fail, whereas a
// semantically drifted one may still work.
func (k DriftKind) Severity() int {
	switch k {
	case DriftRemoved:
		return 3
	case DriftSemantic:
		return 2
	case DriftCosmetic:
		return 1
	case DriftAdded:
		return 0
	default:
		return -1
	}
}

// Blocking reports whether this kind makes a tool unfit to serve in strict
// mode.
//
// Cosmetic drift does not: the admitted description is still an accurate
// description of a tool whose schema is unchanged. Added drift does not either,
// because an unadmitted tool is not served at all.
func (k DriftKind) Blocking() bool {
	return k == DriftSemantic || k == DriftRemoved
}

// ToolDrift is one tool's difference.
type ToolDrift struct {
	// Name as the backend publishes it.
	Name string `json:"name"`
	// QualifiedName as clients see it. Empty for an added tool, which has none
	// — assigning one is admission's job, and doing it here would be exactly
	// the runtime auto-naming the design forbids.
	QualifiedName string    `json:"qualified_name,omitempty"`
	Kind          DriftKind `json:"kind"`

	AdmittedDigest string `json:"admitted_digest,omitempty"`
	ObservedDigest string `json:"observed_digest,omitempty"`

	// Detail names what changed, so a reader does not have to diff two digests
	// by eye to learn that a description was reworded.
	Detail string `json:"detail"`
}

// Diff compares what a backend publishes now against what was admitted.
//
// admitted is keyed by the backend's own tool name, not the qualified name:
// that is the only key both sides share, since the prefix is assigned at
// admission and the backend knows nothing about it.
func Diff(
	admitted map[string]*snapshotpb.ToolDefinition,
	observed map[string]mcp.ToolDigests,
) []ToolDrift {
	var out []ToolDrift

	for name, def := range admitted {
		obs, present := observed[name]
		if !present {
			out = append(out, ToolDrift{
				Name:           name,
				QualifiedName:  def.QualifiedName,
				Kind:           DriftRemoved,
				AdmittedDigest: def.Digest,
				Detail:         "the backend no longer publishes this tool",
			})
			continue
		}

		full := string(obs.Full)
		if full == def.Digest {
			continue
		}

		semantic := string(obs.Semantic)
		if semantic == def.SemanticDigest {
			out = append(out, ToolDrift{
				Name:           name,
				QualifiedName:  def.QualifiedName,
				Kind:           DriftCosmetic,
				AdmittedDigest: def.Digest,
				ObservedDigest: full,
				Detail:         cosmeticDetail(def, obs),
			})
			continue
		}

		out = append(out, ToolDrift{
			Name:           name,
			QualifiedName:  def.QualifiedName,
			Kind:           DriftSemantic,
			AdmittedDigest: def.SemanticDigest,
			ObservedDigest: semantic,
			Detail:         semanticDetail(def, obs),
		})
	}

	for name := range observed {
		if _, wasAdmitted := admitted[name]; !wasAdmitted {
			out = append(out, ToolDrift{
				Name:   name,
				Kind:   DriftAdded,
				Detail: "the backend publishes this tool; it has not been admitted, so it is not served",
			})
		}
	}

	// Most severe first, then by name. Map iteration is randomised, and a
	// report that reshuffles between probes cannot be diffed by a human or
	// deduplicated by an alerting rule.
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Kind.Severity(), out[j].Kind.Severity(); a != b {
			return a > b
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func cosmeticDetail(def *snapshotpb.ToolDefinition, obs mcp.ToolDigests) string {
	var changed []string
	if obs.Definition != nil {
		if obs.Definition.Description != def.Description {
			changed = append(changed, "description")
		}
		if obs.Definition.Title != def.Title {
			changed = append(changed, "title")
		}
	}
	if len(changed) == 0 {
		// The semantic digests match but the full ones do not, and no prose
		// field differs. That means canonicalization found a difference the
		// prose-stripping pass removed but this comparison does not name —
		// worth saying rather than reporting a confident blank.
		return "prose changed; the schema is unchanged"
	}
	return strings.Join(changed, " and ") + " changed; the schema is unchanged"
}

func semanticDetail(def *snapshotpb.ToolDefinition, obs mcp.ToolDigests) string {
	if obs.Definition == nil {
		return "the schema or annotations changed"
	}
	// Both sides are compared as canonical bytes. The admitted side is already
	// canonical — it was stored that way — and the observed side is
	// canonicalized here, so a reordered JSON object is not reported as a
	// change. That distinction is the entire reason canonicalization exists.
	var changed []string
	if !sameCanonical(obs.Definition.InputSchema, def.InputSchemaJson) {
		changed = append(changed, "input schema")
	}
	if !sameCanonical(obs.Definition.OutputSchema, def.OutputSchemaJson) {
		changed = append(changed, "output schema")
	}
	if !sameCanonical(obs.Definition.Annotations, def.AnnotationsJson) {
		changed = append(changed, "annotations")
	}
	if len(changed) == 0 {
		return "the canonical definition changed in a way that is not prose"
	}
	return fmt.Sprintf("%s changed", strings.Join(changed, " and "))
}

// sameCanonical compares an observed value against admitted canonical JSON.
//
// An absent value on either side is the empty string, so a schema that was
// dropped entirely compares unequal rather than erroring.
func sameCanonical(observed any, admitted string) bool {
	if isEmptyValue(observed) {
		return admitted == ""
	}
	encoded, err := canonical.Marshal(observed)
	if err != nil {
		// Unmarshalable means it certainly differs from stored canonical bytes,
		// and reporting "unchanged" on an error would hide the drift this
		// function exists to find.
		return false
	}
	return string(encoded) == admitted
}

func isEmptyValue(v any) bool {
	switch typed := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

// Blocking returns the drifts that make a tool unfit to serve.
func Blocking(drifts []ToolDrift) []ToolDrift {
	var out []ToolDrift
	for _, d := range drifts {
		if d.Kind.Blocking() {
			out = append(out, d)
		}
	}
	return out
}
