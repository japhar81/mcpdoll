// Copyright 2026 The MCPDoll Authors.

package apiserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The commonest way to write a grant that does nothing.
//
// A tool scope names the backend's own tool; every catalog shows the qualified
// name with the namespace prefix on it. An operator naturally types what they
// see, the grant stores fine, compiles fine, and authorizes nothing — and the
// only symptom is an empty catalog with no reason attached. This check is the
// last place the mistake is still attributable to the change that caused it.

func testScopes() knownScopes {
	return knownScopes{
		loaded:   true,
		tenants:  map[string]bool{"acme": true},
		toolsets: map[string]bool{"acme/support": true},
		tools:    map[string]bool{"acme/support/lookup_customer": true},
		qualified: map[string]string{
			"acme/support/crm.lookup_customer": "lookup_customer",
		},
	}
}

func TestAGrantableScopePasses(t *testing.T) {
	t.Parallel()
	k := testScopes()

	for _, scope := range []string{
		"*",
		"t/acme",
		"t/acme/ts/support",
		"t/acme/ts/support/lookup_customer",
	} {
		require.Empty(t, k.check(scope), scope)
	}
}

func TestTheQualifiedNameIsRefusedWithTheAnswer(t *testing.T) {
	t.Parallel()

	problem := testScopes().check("t/acme/ts/support/crm.lookup_customer")
	require.Contains(t, problem, "uses the qualified name")
	// Naming the correct form is the whole value: "invalid scope" would leave
	// the operator to work out that the prefix is the problem.
	require.Contains(t, problem, "lookup_customer")
	require.Contains(t, problem, "namespace prefix is not part of it")
}

func TestAScopeThatMatchesNothingIsRefused(t *testing.T) {
	t.Parallel()
	k := testScopes()

	require.Contains(t, k.check("t/globex"), "does not carry")
	require.Contains(t, k.check("t/acme/ts/platform"), "admits nothing")
	require.Contains(t, k.check("t/acme/ts/support/no_such_tool"), "does not carry")
	require.Contains(t, k.check("not-a-scope"), "malformed")
}

func TestWithoutASnapshotEveryScopePasses(t *testing.T) {
	t.Parallel()

	// A fresh install has published nothing. Refusing grants until it has would
	// make it unusable, and the check is a courtesy — the scope is enforced at
	// serving time whatever this says.
	empty := knownScopes{}
	require.Empty(t, empty.check("t/anything/ts/at/all"))

	// Malformed is still malformed: that one needs no snapshot to know.
	require.Contains(t, empty.check("not-a-scope"), "malformed")
}

func TestASiblingTenantIsNotCoveredByAPrefix(t *testing.T) {
	t.Parallel()
	k := testScopes()
	k.tenants["acme-corp"] = true

	// `t/acme` and `t/acme-corp` share seven characters and are different
	// tenants. Both are grantable and neither covers the other — the boundary
	// check lives in authz.ScopeCovers, and this is the check that they are
	// both *known*, which is what would otherwise mask a typo.
	require.Empty(t, k.check("t/acme"))
	require.Empty(t, k.check("t/acme-corp"))
}
