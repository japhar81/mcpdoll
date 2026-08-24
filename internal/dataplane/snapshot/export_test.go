// Copyright 2026 Henry Zektser.

package snapshot

// Test-only seams.
//
// A PrincipalView is composed from a snapshot and a principal set, which live
// in a Store. The view tests predate that split and assert against a bare
// *View, so these let a fixture hand one the store it was built into rather
// than rewriting every assertion for a two-artifact world.

// fixtureStore is set by the test fixture's build().
func (v *View) SetFixtureStore(s *Store) { v.fixtureStore = s }

// activateForTest installs a view without going through signature verification.
func (s *Store) activateForTest(v *View) {
	s.current.Store(v)
}
