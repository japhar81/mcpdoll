// Copyright 2026 The MCPDoll Authors.

package edge_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/edge"
)

// MRTR — the `resultType: "input_required"` flow — is how a stateless gateway does
// human-in-the-loop. In stateless mode the server cannot make client requests, so
// an interactive step becomes: return an input request, let the client fulfil it,
// and have the client retry with the responses plus the opaque `requestState`.
//
// The gateway's specific job is to wrap the backend's state in its own signed
// envelope. These tests exercise the real round trip, then attack the envelope.

// TestMRTRBackendRoundTrip drives the full two-round flow against a backend that
// genuinely asks for confirmation.
func TestMRTRBackendRoundTrip(t *testing.T) {
	h := newHarness(t, harnessOptions{WithStateSigner: true})
	session := h.Connect(t, "platform-agents", nil)
	ctx := context.Background()

	args := map[string]any{"build": "v2026.8.1"}

	// Round 1: the gateway relays the backend's request for input.
	first, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "dep.promote_release", Arguments: args,
	})
	require.NoError(t, err)
	require.True(t, first.NeedsInput(),
		"the gateway must surface the backend's input request as resultType input_required")
	require.Len(t, first.InputRequests, 1)

	elicit, ok := first.InputRequests["confirm"].(*sdk.ElicitParams)
	require.True(t, ok, "expected an elicitation, got %T", first.InputRequests["confirm"])
	require.Contains(t, elicit.Message, "v2026.8.1",
		"the prompt the human sees comes from the backend, unchanged")

	// The state the client received is the gateway's signed envelope, not the
	// backend's own string handed straight through.
	//
	// The property this buys is *unforgeability*, not confidentiality: the
	// envelope is signed and base64-encoded, so a determined client can decode
	// and read the backend's state, but it cannot produce a valid envelope of its
	// own or redeem this one against a different call. ADR 0012 explains why
	// confidentiality is not needed here — nothing in the envelope is secret.
	require.NotEmpty(t, first.RequestState)
	require.NotEqual(t, "promote:v2026.8.1", first.RequestState,
		"the backend's state must be wrapped, not passed through")
	require.True(t, strings.HasPrefix(first.RequestState, "mcpd1."),
		"the client should receive a signed envelope, got %q", first.RequestState)

	// Round 2: the client fulfils the request and retries.
	second, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "dep.promote_release",
		Arguments: args,
		InputResponses: sdk.InputResponseMap{
			"confirm": &sdk.ElicitResult{Action: "accept"},
		},
		RequestState: first.RequestState,
	})
	require.NoError(t, err)
	require.False(t, second.NeedsInput(), "the retry should complete")
	require.False(t, second.IsError, contentText(second))
	require.Contains(t, contentText(second), "promoted",
		"the promotion must actually have happened")
	require.Contains(t, contentText(second), "promote:v2026.8.1",
		"the backend must have received back its own original state")

	require.Equal(t, 2, h.Confirming.Calls("promote_release"),
		"both rounds reach the backend; the gateway does not answer the first one itself")
}

// TestMRTRDeclineDoesNotAct: a human who says no must not have the action taken.
func TestMRTRDeclineDoesNotAct(t *testing.T) {
	h := newHarness(t, harnessOptions{WithStateSigner: true})
	session := h.Connect(t, "platform-agents", nil)
	ctx := context.Background()
	args := map[string]any{"build": "v9"}

	first, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "dep.promote_release", Arguments: args,
	})
	require.NoError(t, err)
	require.True(t, first.NeedsInput())

	second, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "dep.promote_release",
		Arguments: args,
		InputResponses: sdk.InputResponseMap{
			"confirm": &sdk.ElicitResult{Action: "decline"},
		},
		RequestState: first.RequestState,
	})
	require.NoError(t, err)
	require.Contains(t, contentText(second), "cancelled")
	require.NotContains(t, contentText(second), "promoted")
}

// TestMRTRWithoutSignerIsRefused: serving an unsigned requestState would let a
// client forge its own approval, so the gateway must refuse rather than degrade.
func TestMRTRWithoutSignerIsRefused(t *testing.T) {
	h := newHarness(t, harnessOptions{WithStateSigner: false})
	session := h.Connect(t, "platform-agents", nil)

	_, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "dep.promote_release", Arguments: map[string]any{"build": "v1"},
	})
	require.Error(t, err,
		"without a signer the gateway must refuse the interactive flow rather than emit an unsigned state")
	require.Contains(t, err.Error(), "requestState")
}

// TestMRTRPluginDeferral covers the other source of a deferral: a plugin that
// wants human confirmation before a destructive call.
func TestMRTRPluginDeferral(t *testing.T) {
	h := newHarness(t, harnessOptions{
		WithStateSigner: true,
		Pipeline:        &confirmingPipeline{},
	})
	session := h.Connect(t, "platform-agents", nil)
	ctx := context.Background()

	// A destructive tool: the plugin defers.
	first, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "dep.promote_release", Arguments: map[string]any{"build": "v3"},
	})
	require.NoError(t, err)
	require.True(t, first.NeedsInput(), "the plugin's deferral must become input_required")
	require.Equal(t, 0, h.Confirming.Calls("promote_release"),
		"a deferred call must not reach the backend before the human answers")

	// A read tool: no deferral, straight through.
	read, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "crm.lookup_customer", Arguments: map[string]any{"customer_id": "cus_1"},
	})
	require.NoError(t, err)
	require.False(t, read.NeedsInput(), "a read must not be gated on confirmation")
	require.False(t, read.IsError, contentText(read))
}

// confirmingPipeline defers destructive calls for human confirmation.
type confirmingPipeline struct{}

func (p *confirmingPipeline) OnCatalog(_ context.Context, req *edge.CatalogRequest) (*edge.CatalogDecision, error) {
	return &edge.CatalogDecision{}, nil
}

func (p *confirmingPipeline) OnToolCall(_ context.Context, req *edge.ToolCallRequest) (*edge.ToolCallDecision, error) {
	if req.Tool.Def.EffectClass.String() != "EFFECT_CLASS_DESTRUCTIVE" {
		return &edge.ToolCallDecision{Decision: "allow"}, nil
	}
	// Already answered: let it through.
	if len(req.InputResponses) > 0 {
		return &edge.ToolCallDecision{Decision: "allow"}, nil
	}
	return &edge.ToolCallDecision{
		Decision: "defer",
		Reason:   "destructive tools require confirmation",
		InputRequests: sdk.InputRequestMap{
			"approve": &sdk.ElicitParams{
				Message: "This is a destructive operation. Proceed?",
			},
		},
		RequestState: "policy-confirm",
	}, nil
}

func (p *confirmingPipeline) OnToolResult(context.Context, *edge.ToolResultRequest) (*edge.ToolResultDecision, error) {
	return &edge.ToolResultDecision{Decision: "allow"}, nil
}

// ------------------------------------------------ requestState envelope ------

// TestStateSignerRejectsForgery attacks the envelope directly. Each case is a
// way a client might try to manufacture an approval it was never given.
func TestStateSignerRejectsForgery(t *testing.T) {
	key, err := edge.GenerateStateKey()
	require.NoError(t, err)
	signer, err := edge.NewStateSigner(key)
	require.NoError(t, err)

	valid, err := signer.WrapForTest("platform-agents", "dep.promote_release",
		"alice@example.com", map[string]any{"build": "v1"}, "backend-state", "")
	require.NoError(t, err)

	// The honest path works.
	env, err := signer.UnwrapForTest(valid)
	require.NoError(t, err)
	require.Equal(t, "backend-state", env.Backend)
	require.Equal(t, "dep.promote_release", env.Tool)

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"not an envelope", "just-a-string"},
		{"wrong version prefix", "mcpd9." + valid[len("mcpd1."):]},
		{"missing signature", valid[:len(valid)-45]},
		{"flipped signature byte", flipSignatureByte(t, valid)},
		{"flipped payload byte", flipPayloadChar(valid)},
		{"payload swapped for another", swapPayload(t, signer, valid)},
		{"signature stripped entirely", stripAfterSecondDot(valid)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := signer.UnwrapForTest(tc.token)
			require.Error(t, err, "forged token %q must be refused", tc.token)
		})
	}
}

// TestStateSignerRejectsAnotherKeysEnvelope: an envelope minted by a different
// gateway (or a different key generation) must not verify.
func TestStateSignerRejectsAnotherKeysEnvelope(t *testing.T) {
	keyA, err := edge.GenerateStateKey()
	require.NoError(t, err)
	signerA, err := edge.NewStateSigner(keyA)
	require.NoError(t, err)

	keyB, err := edge.GenerateStateKey()
	require.NoError(t, err)
	signerB, err := edge.NewStateSigner(keyB)
	require.NoError(t, err)

	token, err := signerA.WrapForTest("aud", "tool", "sub", nil, "state", "")
	require.NoError(t, err)

	_, err = signerB.UnwrapForTest(token)
	require.ErrorIs(t, err, edge.ErrInvalidRequestState)
}

func TestNewStateSignerRejectsShortKey(t *testing.T) {
	_, err := edge.NewStateSigner([]byte("too-short"))
	require.ErrorContains(t, err, "at least 32")
	// The message should say *why* it matters, not just state the rule.
	require.ErrorContains(t, err, "forging")
}

// flipSignatureByte flips a byte of the *decoded* signature and re-encodes.
//
// Flipping a base64url character directly is not reliable: a 64-byte signature
// encodes to 86 characters, so the last one carries four unused bits and several
// distinct characters decode to the same bytes. Mutating the decoded form makes
// the tamper unambiguous.
func flipSignatureByte(t *testing.T, token string) string {
	t.Helper()
	dot := lastIndexByte(token, '.')
	require.Positive(t, dot)
	sig, err := base64.RawURLEncoding.DecodeString(token[dot+1:])
	require.NoError(t, err)
	sig[0] ^= 0xFF
	return token[:dot+1] + base64.RawURLEncoding.EncodeToString(sig)
}

// flipPayloadChar mutates the signed payload, which the MAC covers.
func flipPayloadChar(token string) string {
	first := indexByte(token, '.')
	last := lastIndexByte(token, '.')
	if first < 0 || last <= first+2 {
		return token
	}
	b := []byte(token)
	mid := first + 2
	if b[mid] == 'A' {
		b[mid] = 'B'
	} else {
		b[mid] = 'A'
	}
	return string(b)
}

func stripAfterSecondDot(s string) string {
	first := indexByte(s, '.')
	if first < 0 {
		return s
	}
	second := indexByte(s[first+1:], '.')
	if second < 0 {
		return s
	}
	return s[:first+1+second]
}

// swapPayload keeps a valid signature but substitutes a payload signed for a
// different call — the replay attempt that binding exists to stop.
func swapPayload(t *testing.T, signer *edge.StateSigner, valid string) string {
	t.Helper()
	other, err := signer.WrapForTest("aud", "some.other_tool", "bob", nil, "other-state", "")
	require.NoError(t, err)

	validSig := valid[lastIndexByte(valid, '.'):]
	otherHead := other[:lastIndexByte(other, '.')]
	return otherHead + validSig
}

func indexByte(s string, c byte) int {
	for i := range len(s) {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// TestMRTRPluginDeferralStateIsSigned: a plugin's deferral must reach the client
// wrapped, not raw. An unsigned state would let a client fabricate the approval
// the plugin was waiting for.
func TestMRTRPluginDeferralStateIsSigned(t *testing.T) {
	h := newHarness(t, harnessOptions{
		WithStateSigner: true,
		Pipeline:        &confirmingPipeline{},
	})
	session := h.Connect(t, "platform-agents", nil)

	res, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name: "dep.promote_release", Arguments: map[string]any{"build": "v7"},
	})
	require.NoError(t, err)
	require.True(t, res.NeedsInput())
	require.NotEqual(t, "policy-confirm", res.RequestState,
		"the plugin's raw state must not be handed to the client")
	require.NotContains(t, res.RequestState, "policy-confirm")
	require.True(t, strings.HasPrefix(res.RequestState, "mcpd1."),
		"the client should receive a signed envelope, got %q", res.RequestState)
}

// TestMRTRStateCannotBeReplayedAgainstDifferentArguments is the binding the
// envelope exists for: an approval for one target must not authorize another.
func TestMRTRStateCannotBeReplayedAgainstDifferentArguments(t *testing.T) {
	h := newHarness(t, harnessOptions{
		WithStateSigner: true,
		Pipeline:        &confirmingPipeline{},
	})
	session := h.Connect(t, "platform-agents", nil)
	ctx := context.Background()

	// Get a genuine approval token for build v1.
	first, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name: "dep.promote_release", Arguments: map[string]any{"build": "v1"},
	})
	require.NoError(t, err)
	require.True(t, first.NeedsInput())

	// Replay it against a different build.
	replayed, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "dep.promote_release",
		Arguments: map[string]any{"build": "v2-PRODUCTION"},
		InputResponses: sdk.InputResponseMap{
			"approve": &sdk.ElicitResult{Action: "accept"},
		},
		RequestState: first.RequestState,
	})
	require.NoError(t, err)
	require.True(t, replayed.IsError, "a replayed approval must be refused")
	require.Contains(t, contentText(replayed), "could not be verified")
	require.Equal(t, 0, h.Confirming.Calls("promote_release"),
		"the replayed approval must not have reached the backend")

	// And the honest retry, with the arguments it was issued for, still works.
	honest, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "dep.promote_release",
		Arguments: map[string]any{"build": "v1"},
		InputResponses: sdk.InputResponseMap{
			"approve": &sdk.ElicitResult{Action: "accept"},
		},
		RequestState: first.RequestState,
	})
	require.NoError(t, err)
	require.False(t, honest.IsError, contentText(honest))
}

// TestMRTRBackendStateIsBoundToTheCall: a backend's approval must be no more
// replayable than a plugin's. An approval for "promote v1" must not authorize
// promoting a different build, whoever asked the question.
func TestMRTRBackendStateIsBoundToTheCall(t *testing.T) {
	h := newHarness(t, harnessOptions{WithStateSigner: true})
	ctx := context.Background()

	alice := http.Header{}
	alice.Set(edge.HeaderSubject, "alice@example.com")
	aliceSession := h.Connect(t, "platform-agents", alice)

	first, err := aliceSession.CallTool(ctx, &sdk.CallToolParams{
		Name: "dep.promote_release", Arguments: map[string]any{"build": "v1"},
	})
	require.NoError(t, err)
	require.True(t, first.NeedsInput())

	t.Run("replayed against different arguments", func(t *testing.T) {
		res, err := aliceSession.CallTool(ctx, &sdk.CallToolParams{
			Name:      "dep.promote_release",
			Arguments: map[string]any{"build": "v2-PRODUCTION"},
			InputResponses: sdk.InputResponseMap{
				"confirm": &sdk.ElicitResult{Action: "accept"},
			},
			RequestState: first.RequestState,
		})
		require.NoError(t, err)
		require.True(t, res.IsError, "an approval for v1 must not authorize v2")
		require.Contains(t, contentText(res), "could not be verified")
	})

	t.Run("replayed by a different principal", func(t *testing.T) {
		bob := http.Header{}
		bob.Set(edge.HeaderSubject, "bob@example.com")
		bobSession := h.Connect(t, "platform-agents", bob)

		res, err := bobSession.CallTool(ctx, &sdk.CallToolParams{
			Name:      "dep.promote_release",
			Arguments: map[string]any{"build": "v1"},
			InputResponses: sdk.InputResponseMap{
				"confirm": &sdk.ElicitResult{Action: "accept"},
			},
			RequestState: first.RequestState,
		})
		require.NoError(t, err)
		require.True(t, res.IsError, "Alice's approval must not be redeemable by Bob")
		require.Contains(t, contentText(res), "could not be verified")
	})

	// The honest retry, same principal and same arguments, still works.
	honest, err := aliceSession.CallTool(ctx, &sdk.CallToolParams{
		Name:      "dep.promote_release",
		Arguments: map[string]any{"build": "v1"},
		InputResponses: sdk.InputResponseMap{
			"confirm": &sdk.ElicitResult{Action: "accept"},
		},
		RequestState: first.RequestState,
	})
	require.NoError(t, err)
	require.False(t, honest.IsError, contentText(honest))
	require.Contains(t, contentText(honest), "promoted")
}
