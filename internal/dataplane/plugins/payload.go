// Copyright 2026 Henry Zektser.

package plugins

import (
	"encoding/json"
	"fmt"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/pipeline"
	snapshotpb "github.com/mcpdoll/mcpdoll/internal/proto/snapshotpb"
)

// buildPayload assembles what a plugin actually receives.
//
// The engine builds the *request* half — who is asking, which tool, what the
// result was — because that is request state. The host adds the *plugin* half:
// its configuration, the hook name, and whether this invocation is shadowed.
//
// Splitting it this way keeps the engine free of anything plugin-specific, and
// means both hosts assemble the payload identically. A WASM plugin and a gRPC
// plugin reading the same manifest must see the same configuration, or the
// runtime becomes part of a plugin's contract.
func buildPayload(inv *pipeline.Invocation) ([]byte, error) {
	// Start from the engine's request state.
	var payload map[string]any
	if len(inv.Context) > 0 {
		if err := json.Unmarshal(inv.Context, &payload); err != nil {
			return nil, fmt.Errorf("plugins: invocation context is not a JSON object: %w", err)
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}

	// The hook and shadow flag are the host's to state: a plugin must not have to
	// infer which hook it is running at, and it cannot know on its own whether
	// its verdict will be enforced.
	payload["hook"] = hookName(inv.Hook)
	payload["shadow"] = inv.Shadow

	if config := inv.Manifest.GetConfigJson(); config != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(config), &parsed); err != nil {
			// A manifest whose config will not parse is a control-plane bug. It
			// surfaces as a plugin failure so the engine's failure policy decides
			// what to do, rather than the plugin silently running unconfigured —
			// which for a redaction plugin means redacting nothing.
			return nil, fmt.Errorf("plugins: %s has malformed config JSON: %w",
				inv.Manifest.Name, err)
		}
		payload["config"] = parsed
	}

	return json.Marshal(payload)
}

func hookName(h snapshotpb.Hook) string {
	switch h {
	case snapshotpb.Hook_HOOK_ON_REQUEST:
		return "on_request"
	case snapshotpb.Hook_HOOK_ON_IDENTITY:
		return "on_identity"
	case snapshotpb.Hook_HOOK_ON_CATALOG:
		return "on_catalog"
	case snapshotpb.Hook_HOOK_ON_TOOL_CALL:
		return "on_tool_call"
	case snapshotpb.Hook_HOOK_ON_TOOL_RESULT:
		return "on_tool_result"
	case snapshotpb.Hook_HOOK_ON_RESPONSE:
		return "on_response"
	case snapshotpb.Hook_HOOK_ON_AUDIT:
		return "on_audit"
	default:
		return "unspecified"
	}
}
