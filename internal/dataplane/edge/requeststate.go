// Copyright 2026 Henry Zektser.

package edge

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
)

// MRTR — the multi-round-trip flow behind `resultType: "input_required"` — is
// how a stateless gateway does human-in-the-loop. Because the server cannot make
// client requests in stateless mode, an interactive step becomes: return an input
// request, let the client fulfil it, and have the client retry the call with the
// responses and an opaque `requestState` echoed back.
//
// That `requestState` round-trips through an untrusted client, which makes it the
// security-critical part of the flow. The spec says an unauthenticated server
// must encrypt, sign, and verify it. MCPDoll signs it, and wraps the backend's
// own state inside its envelope, for three reasons:
//
//  1. The client must not be able to forge state. Without a signature, a client
//     could fabricate a `requestState` claiming an approval that never happened
//     — which for a destructive tool means self-authorizing the destruction.
//
//  2. The state must be bound to the call it approves. An approval for
//     "void invoice INV-1" replayed against "void invoice INV-2" would be a
//     confused-deputy bug, so the envelope carries the tool and an argument
//     digest and the retry is checked against them.
//
//  3. The backend's state must not leak the gateway's, or vice versa. The
//     backend gets back exactly the bytes it produced and never sees the
//     envelope; the client sees only an opaque blob.
//
// The envelope also expires, because an approval that is valid forever is a
// standing authorization nobody remembers granting.

// StateTTL is how long a wrapped requestState stays valid.
const StateTTL = 10 * time.Minute

// Deferral sources. See stateEnvelope.Source.
const (
	SourceBackend = "backend"
	SourcePlugin  = "plugin"
)

// stateVersion prefixes the envelope so a future format change is detectable
// rather than a parse error.
const stateVersion = "mcpd1"

// ErrInvalidRequestState reports a requestState that failed verification.
var ErrInvalidRequestState = errors.New("edge: requestState failed verification")

// ErrExpiredRequestState reports a requestState past its TTL.
var ErrExpiredRequestState = errors.New("edge: requestState has expired")

// ErrMismatchedRequestState reports a requestState presented against a
// different call than the one it was issued for.
var ErrMismatchedRequestState = errors.New("edge: requestState does not match this call")

// stateEnvelope is the signed payload.
type stateEnvelope struct {
	Version string `json:"v"`
	// Tenant, Tool and Principal bind the state to who may redeem it and for
	// what.
	// The JSON key stays `aud`. It is the conventional name for this claim,
	// and it is inside a signature — renaming it would invalidate every
	// envelope in flight for no gain.
	Tenant    string `json:"aud"`
	Tool      string `json:"tool"`
	Principal string `json:"sub"`
	// ArgsDigest binds the state to the specific arguments, so an approval
	// cannot be replayed against a different target.
	ArgsDigest string `json:"args"`
	// Source records who asked for the input: "backend" or "plugin".
	//
	// This is not redundant with which state field is populated. A deferral's
	// input *responses* must be routed back to whoever requested them, because
	// the response keys are assigned by the requester and the two namespaces are
	// independent. Handing a plugin's responses to a backend makes the backend
	// think it was answered when it was not — it sees a non-empty response map,
	// takes its second-round branch, and finds none of its own keys.
	Source string `json:"src,omitempty"`
	// Backend is the backend's own opaque state, passed through untouched.
	Backend string `json:"bs,omitempty"`
	// Plugin is a deferring plugin's opaque state.
	Plugin string `json:"ps,omitempty"`
	// IssuedAt and Nonce give the envelope a lifetime and make two otherwise
	// identical envelopes distinguishable in an audit trail.
	IssuedAt int64  `json:"iat"`
	Nonce    string `json:"n"`
}

// StateSigner signs and verifies requestState envelopes.
//
// HMAC-SHA256 rather than Ed25519: the gateway is both issuer and verifier, so
// there is no third party who needs to check a signature, and a symmetric MAC is
// materially cheaper on a path that runs per interactive call.
type StateSigner struct {
	key []byte
}

// NewStateSigner builds a signer from a secret. The key must be at least 32
// bytes: a short key would make forgery feasible, and this is the only thing
// standing between a client and a self-issued approval.
func NewStateSigner(key []byte) (*StateSigner, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf(
			"edge: requestState signing key is %d bytes, need at least 32; "+
				"this key is what stops a client forging its own approvals", len(key))
	}
	return &StateSigner{key: append([]byte(nil), key...)}, nil
}

// GenerateStateKey mints a random signing key.
//
// Each data-plane instance may hold its own: a requestState is redeemed by
// whichever instance issued it only if they share the key, so a multi-instance
// deployment must configure a shared secret. That is a deployment requirement,
// documented in docs/operations/, not something the gateway can paper over —
// silently accepting a state it cannot verify would defeat the whole mechanism.
func GenerateStateKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("edge: generating requestState key: %w", err)
	}
	return key, nil
}

// Wrap signs an envelope and returns the opaque token the client echoes back.
func (s *StateSigner) Wrap(env stateEnvelope) (string, error) {
	env.Version = stateVersion
	if env.IssuedAt == 0 {
		env.IssuedAt = time.Now().UTC().Unix()
	}
	if env.Nonce == "" {
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			return "", fmt.Errorf("edge: generating requestState nonce: %w", err)
		}
		env.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	}

	payload, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("edge: encoding requestState: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := s.sign(encoded)
	return stateVersion + "." + encoded + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

// Unwrap verifies a token and returns its envelope.
func (s *StateSigner) Unwrap(token string) (stateEnvelope, error) {
	var env stateEnvelope

	version, rest, ok := cut(token, ".")
	if !ok || version != stateVersion {
		return env, fmt.Errorf("%w: unexpected format", ErrInvalidRequestState)
	}
	encoded, macPart, ok := cut(rest, ".")
	if !ok {
		return env, fmt.Errorf("%w: missing signature", ErrInvalidRequestState)
	}

	gotMAC, err := base64.RawURLEncoding.DecodeString(macPart)
	if err != nil {
		return env, fmt.Errorf("%w: signature is not base64", ErrInvalidRequestState)
	}
	// Constant-time compare: a timing-variable check on a MAC is a forgery
	// oracle.
	if !hmac.Equal(gotMAC, s.sign(encoded)) {
		return env, ErrInvalidRequestState
	}

	// Only decode after the MAC verifies, so unauthenticated bytes never reach
	// the JSON parser.
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return env, fmt.Errorf("%w: payload is not base64", ErrInvalidRequestState)
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return env, fmt.Errorf("%w: payload is not valid JSON", ErrInvalidRequestState)
	}
	if env.Version != stateVersion {
		return env, fmt.Errorf("%w: envelope version %q", ErrInvalidRequestState, env.Version)
	}

	issued := time.Unix(env.IssuedAt, 0).UTC()
	if time.Since(issued) > StateTTL {
		return env, fmt.Errorf("%w (issued %s)", ErrExpiredRequestState, issued.Format(time.RFC3339))
	}
	// A future-dated envelope means either clock skew beyond tolerance or a
	// forgery attempt; either way it is not something to honour.
	if time.Until(issued) > time.Minute {
		return env, fmt.Errorf("%w: issued in the future", ErrInvalidRequestState)
	}

	return env, nil
}

func (s *StateSigner) sign(encoded string) []byte {
	mac := hmac.New(sha256.New, s.key)
	// Domain-separate from any other HMAC the gateway might compute with this
	// key.
	mac.Write([]byte("mcpdoll.requeststate.v1\x00"))
	mac.Write([]byte(encoded))
	return mac.Sum(nil)
}

func cut(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// argsDigest binds an envelope to the arguments it approves.
func argsDigest(arguments any) string {
	raw, err := json.Marshal(arguments)
	if err != nil {
		// An unmarshalable argument set cannot be bound; use a sentinel that
		// will never match a real digest so the retry is refused rather than
		// silently accepted.
		return "unbindable"
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ------------------------------------------------------------ edge wiring ----

// wrapBackendInputRequest turns a backend's input-required result into one the
// client can safely act on.
//
// The backend's `requestState` goes inside the gateway's signed envelope, bound
// to the tool, the principal, the tenant, and the argument digest. The client
// cannot forge the envelope and cannot redeem it against a different call — and
// the backend, on retry, gets back exactly the bytes it produced.
//
// The bindings are the same set a plugin deferral gets. There is no reason for a
// backend's approval to be more replayable than a plugin's: an approval for
// "promote build v1" must not authorize promoting v2, whoever asked the question.
func (e *Edge) wrapBackendInputRequest(
	tool *snapshot.Tool,
	principal string,
	tenant string,
	arguments any,
	res *mcp.CallToolResult,
) (*mcp.CallToolResult, error) {
	if e.opts.StateSigner == nil {
		return nil, errors.New(
			"edge: a backend requested client input but no requestState signer is configured; " +
				"an unsigned state would let a client forge its own approval")
	}
	token, err := e.opts.StateSigner.Wrap(stateEnvelope{
		Tenant:     tenant,
		Tool:       tool.Def.QualifiedName,
		Principal:  principal,
		ArgsDigest: argsDigest(arguments),
		Source:     SourceBackend,
		Backend:    res.RequestState,
	})
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{
		InputRequests: res.InputRequests,
		RequestState:  token,
		Meta:          res.Meta,
	}, nil
}

// UnwrapForRetry verifies an inbound requestState and returns the backend's
// original state.
//
// Called on an MRTR retry before dispatch. A verification failure must abort the
// call: proceeding without the state would drop the binding between the approval
// and what it approved.
func (e *Edge) UnwrapForRetry(
	tool *snapshot.Tool,
	principal string,
	tenant string,
	arguments any,
	token string,
) (retry Retry, err error) {
	if token == "" {
		return Retry{}, nil
	}
	if e.opts.StateSigner == nil {
		return Retry{}, errors.New("edge: requestState presented but no signer is configured")
	}
	env, err := e.opts.StateSigner.Unwrap(token)
	if err != nil {
		return Retry{}, err
	}
	if env.Tool != "" && env.Tool != tool.Def.QualifiedName {
		return Retry{}, fmt.Errorf("%w: issued for %q, presented for %q",
			ErrMismatchedRequestState, env.Tool, tool.Def.QualifiedName)
	}
	// Subject and tenant are checked only when the envelope carries them, so
	// a backend-only wrap (which has no principal binding) still round-trips.
	if env.Principal != "" && env.Principal != principal {
		return Retry{}, fmt.Errorf("%w: issued for a different principal", ErrMismatchedRequestState)
	}
	if env.Tenant != "" && env.Tenant != tenant {
		return Retry{}, fmt.Errorf("%w: issued for a different tenant", ErrMismatchedRequestState)
	}
	if env.ArgsDigest != "" && env.ArgsDigest != argsDigest(arguments) {
		return Retry{}, fmt.Errorf("%w: the arguments differ from those approved",
			ErrMismatchedRequestState)
	}
	return Retry{
		Source:       env.Source,
		BackendState: env.Backend,
		PluginState:  env.Plugin,
	}, nil
}

// Retry is a verified MRTR retry.
type Retry struct {
	// Source is who asked for the input: SourceBackend or SourcePlugin.
	Source string
	// BackendState is the backend's own opaque state, to be echoed back to it.
	BackendState string
	// PluginState is the deferring plugin's opaque state.
	PluginState string
}

// AnswersBackend reports whether the client's input responses belong to the
// backend, and may therefore be forwarded to it.
func (r Retry) AnswersBackend() bool { return r.Source == SourceBackend }

// AnswersPlugin reports whether the responses belong to a plugin.
func (r Retry) AnswersPlugin() bool { return r.Source == SourcePlugin }

// IssuePluginDeferral wraps a deferring plugin's state, binding it to the call.
//
// Unlike a backend wrap this binds the principal, tenant, and arguments,
// because a plugin deferral is usually an authorization decision about a
// specific action by a specific person — exactly the thing that must not be
// replayable.
func (e *Edge) IssuePluginDeferral(
	tool *snapshot.Tool,
	principal string,
	tenant string,
	arguments any,
	pluginState string,
) (string, error) {
	if e.opts.StateSigner == nil {
		return "", errors.New(
			"edge: a plugin deferred but no requestState signer is configured")
	}
	return e.opts.StateSigner.Wrap(stateEnvelope{
		Tenant:     tenant,
		Tool:       tool.Def.QualifiedName,
		Principal:  principal,
		ArgsDigest: argsDigest(arguments),
		Source:     SourcePlugin,
		Plugin:     pluginState,
	})
}
