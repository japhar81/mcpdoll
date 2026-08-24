// Copyright 2026 Henry Zektser.

package edge

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
	"github.com/mcpdoll/mcpdoll/internal/dataplane/snapshot"
)

// IdentityResolver turns an inbound request into the principal on whose behalf
// the gateway is acting.
//
// It is an interface so the IdP side is swappable and so the data plane can be
// tested without an identity provider. The resolver returns a principal and
// nothing else: notably, it does not return the inbound credential. A principal
// is what the rest of the pipeline needs, and not carrying the raw token past
// this boundary is what makes token passthrough structurally impossible rather
// than merely discouraged.
type IdentityResolver interface {
	Resolve(header http.Header) (backends.Principal, error)
}

// ErrUnauthenticated reports a request with no usable identity.
var ErrUnauthenticated = errors.New("edge: request carries no usable identity")

// ErrForbidden reports a principal that may not use this audience.
type ErrForbidden struct {
	Subject  string
	Audience string
	Reason   string
}

func (e *ErrForbidden) Error() string {
	return fmt.Sprintf("edge: principal %q may not use audience %q: %s",
		e.Subject, e.Audience, e.Reason)
}

// Response headers naming who a credential resolved to.
//
// On the response rather than derivable from the request: the tenant comes from
// the key (ADR 0019), so the caller genuinely does not know it until the
// gateway says so. The console's inspection screens read these instead of
// scraping the `instructions` prose, which is written for a model and will be
// reworded.
const (
	HeaderResolvedTenant  = "X-MCPDoll-Tenant"
	HeaderResolvedSubject = "X-MCPDoll-Subject-Resolved"
)

// Dev-mode identity headers. These are read only by [HeaderIdentityResolver].
const (
	HeaderSubject = "X-MCPDoll-Subject"
	HeaderGroups  = "X-MCPDoll-Groups"
	HeaderClaim   = "X-MCPDoll-Claim-"
)

// HeaderIdentityResolver reads identity straight from request headers.
//
// This is for local development and tests. It is *not* an authentication
// mechanism — any client can claim any subject — so it refuses to be
// constructed outside a development environment. Making that a constructor
// error rather than a documented caveat is deliberate: a header-trusting
// resolver reaching production would be a total authorization bypass, and a
// comment is not a control.
type HeaderIdentityResolver struct {
	// DefaultSubject is used when no subject header is present. Leave it empty
	// when this resolver sits behind another in a chain: a default there would
	// turn every failed authentication into a successful one as somebody else,
	// including a request that presented a *wrong* API key.
	DefaultSubject string
	DefaultGroups  []string
}

// NewHeaderIdentityResolver builds a dev resolver. env must be a
// non-production environment.
func NewHeaderIdentityResolver(env string, defaultSubject string, defaultGroups []string) (*HeaderIdentityResolver, error) {
	switch strings.ToLower(env) {
	case "production", "prod":
		return nil, fmt.Errorf(
			"edge: the header identity resolver trusts client-supplied headers and must never run in %q; "+
				"configure a real identity provider", env)
	}
	return &HeaderIdentityResolver{
		DefaultSubject: defaultSubject,
		DefaultGroups:  defaultGroups,
	}, nil
}

// Resolve implements IdentityResolver.
func (r *HeaderIdentityResolver) Resolve(header http.Header) (backends.Principal, error) {
	subject := header.Get(HeaderSubject)
	if subject == "" {
		subject = r.DefaultSubject
	}
	if subject == "" {
		return backends.Principal{}, ErrUnauthenticated
	}

	groups := r.DefaultGroups
	if raw := header.Get(HeaderGroups); raw != "" {
		groups = splitAndTrim(raw)
	}

	claims := map[string]string{}
	for name, values := range header {
		if !strings.HasPrefix(http.CanonicalHeaderKey(name), http.CanonicalHeaderKey(HeaderClaim)) {
			continue
		}
		key := strings.ToLower(strings.TrimPrefix(http.CanonicalHeaderKey(name), http.CanonicalHeaderKey(HeaderClaim)))
		if key != "" && len(values) > 0 {
			claims[key] = values[0]
		}
	}

	// The snapshot addresses principals by id, and this resolver has no
	// directory to look one up in — so the subject *is* the id here. A real
	// resolver (an API key, an OIDC token) maps to the id the control plane
	// published, which is what makes grants findable.
	return backends.Principal{
		ID:      subject,
		Subject: subject,
		Groups:  groups,
		Claims:  claims,
	}, nil
}

func splitAndTrim(raw string) []string {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// APIKeyIdentityResolver authenticates `mcpd.<prefix>.<secret>` against the
// serving snapshot.
//
// No database and no call to the control plane: the snapshot carries each
// active key's prefix and the SHA-256 of its secret, so verification is one map
// lookup and one hash (ADR 0021). That is what makes a control-plane outage
// invisible to a tool call, and it is the reason the digest is not Argon2id —
// a memory-hard KDF on this path would be a denial-of-service primitive pointed
// at ourselves.
type APIKeyIdentityResolver struct {
	// current returns the snapshot to verify against. A function rather than a
	// value because the snapshot is swapped underneath: a resolver holding one
	// view would keep authenticating keys a later snapshot revoked.
	current func() *snapshot.View
}

// NewAPIKeyIdentityResolver builds a resolver over a snapshot source.
func NewAPIKeyIdentityResolver(current func() *snapshot.View) (*APIKeyIdentityResolver, error) {
	if current == nil {
		return nil, errors.New("edge: an API key resolver needs a snapshot source")
	}
	return &APIKeyIdentityResolver{current: current}, nil
}

// Resolve implements IdentityResolver.
//
// Every failure returns the same error. A caller learns whether their
// credential worked and nothing else — not whether the prefix existed, not
// whether the snapshot is stale, not whether the key was revoked. Those
// distinctions are in the log, where the operator can see them and an attacker
// cannot.
func (r *APIKeyIdentityResolver) Resolve(header http.Header) (backends.Principal, error) {
	presented := strings.TrimSpace(
		strings.TrimPrefix(header.Get("Authorization"), "Bearer "))
	if presented == "" {
		return backends.Principal{}, ErrUnauthenticated
	}

	view := r.current()
	if view == nil {
		return backends.Principal{}, ErrUnauthenticated
	}

	prefix, secret, err := splitPresentedKey(presented)
	if err != nil {
		return backends.Principal{}, ErrUnauthenticated
	}

	principal, ok := view.PrincipalByKeyPrefix(prefix)
	if !ok {
		// Hash anyway. An unknown prefix returning before the comparison would
		// be measurably faster than a wrong secret, which is an oracle for
		// enumerating which prefixes exist.
		_ = verifyKeyDigest(secret, "")
		return backends.Principal{}, ErrUnauthenticated
	}
	if !verifyKeyDigest(secret, principal.KeySecretSha256) {
		return backends.Principal{}, ErrUnauthenticated
	}

	tenant := view.TenantByID(principal.TenantId)
	if tenant == nil {
		return backends.Principal{}, ErrUnauthenticated
	}

	// No groups and no claims. They come from an identity provider, and an API
	// key is not one — a key that could assert group membership would let a
	// credential grant itself whatever a group-conditioned policy allows.
	return backends.Principal{
		ID:      principal.Id,
		Subject: principal.Subject,
		Tenant:  tenant.Slug,
	}, nil
}

// splitPresentedKey parses `mcpd.<prefix>.<secret>`.
//
// Duplicated from the store rather than imported: the data plane must not
// depend on the control plane's database package, and the format is a wire
// contract that a shared import would not make more stable. The dot separator
// matters — the fields are base64url, whose alphabet contains both `-` and `_`.
func splitPresentedKey(presented string) (prefix, secret string, err error) {
	parts := strings.Split(presented, ".")
	if len(parts) != 3 || parts[0] != "mcpd" || parts[1] == "" || parts[2] == "" {
		return "", "", ErrUnauthenticated
	}
	return parts[1], parts[2], nil
}

// verifyKeyDigest compares a secret against a stored `sha256:<hex>` digest.
func verifyKeyDigest(secret, stored string) bool {
	sum := sha256.Sum256([]byte(secret))
	computed := "sha256:" + hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(stored)) == 1
}

// ChainIdentityResolvers tries each resolver in order.
//
// Order is the security property, not a convenience: a real credential must be
// checked before any fallback, or a client could present a valid key *and* a
// forged subject header and have the header win. The chain is only ever
// constructed with a development resolver at the end, and never in production.
func ChainIdentityResolvers(resolvers ...IdentityResolver) IdentityResolver {
	return chainResolver(resolvers)
}

type chainResolver []IdentityResolver

func (c chainResolver) Resolve(header http.Header) (backends.Principal, error) {
	for _, r := range c {
		principal, err := r.Resolve(header)
		if err == nil {
			return principal, nil
		}
	}
	return backends.Principal{}, ErrUnauthenticated
}
