// Copyright 2026 Henry Zektser.

package edge

// This file exposes internals to the package's external test binary
// (`package edge_test`), using Go's convention that `_test.go` files can widen a
// package's surface for its own tests only.
//
// The requestState envelope is unexported on purpose: it is a security-critical
// wire format and nothing outside this package has any business constructing
// one. But its resistance to forgery is exactly what must be tested directly
// rather than only through the two-round MCP flow, where a signature failure and
// a protocol failure look alike.

// TestEnvelope is a readable view of a verified requestState envelope.
type TestEnvelope struct {
	Tenant     string
	Tool       string
	Principal  string
	ArgsDigest string
	Backend    string
	Plugin     string
}

// WrapForTest mints an envelope with explicit bindings.
func (s *StateSigner) WrapForTest(
	tenant, tool, principal string,
	arguments any,
	backendState, pluginState string,
) (string, error) {
	env := stateEnvelope{
		Tenant:    tenant,
		Tool:      tool,
		Principal: principal,
		Backend:   backendState,
		Plugin:    pluginState,
	}
	if arguments != nil {
		env.ArgsDigest = argsDigest(arguments)
	}
	return s.Wrap(env)
}

// UnwrapForTest verifies a token and returns its contents.
func (s *StateSigner) UnwrapForTest(token string) (TestEnvelope, error) {
	env, err := s.Unwrap(token)
	if err != nil {
		return TestEnvelope{}, err
	}
	return TestEnvelope{
		Tenant:     env.Tenant,
		Tool:       env.Tool,
		Principal:  env.Principal,
		ArgsDigest: env.ArgsDigest,
		Backend:    env.Backend,
		Plugin:     env.Plugin,
	}, nil
}

// StateTTLForTest exposes the envelope lifetime.
const StateTTLForTest = StateTTL
