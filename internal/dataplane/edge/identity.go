// Copyright 2026 The MCPDoll Authors.

package edge

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mcpdoll/mcpdoll/internal/dataplane/backends"
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
	// DefaultSubject is used when no subject header is present, so a bare
	// `curl` against a dev gateway still works.
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

	return backends.Principal{
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
