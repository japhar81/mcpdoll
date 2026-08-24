// Copyright 2026 Henry Zektser.

package pipeline

// SampleForTest exposes the default canary sampler.
//
// The sampler's *stability* is the property that matters — a request must land
// on the same side of the sample at every hook, or a canary plugin could deny at
// ON_TOOL_CALL and abstain at ON_TOOL_RESULT, leaving a half-enforced request
// nobody can reason about. That is worth testing directly rather than only
// through an engine.
func SampleForTest(requestID string, percent int32) bool {
	return hashSample(requestID, percent)
}
