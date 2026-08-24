// Copyright 2026 Henry Zektser.

//go:build wasip1 && wasm

package sdk

import "unsafe"

// This file is the entire ABI surface, and it exists so no plugin author ever
// writes it.
//
// The build tag matters: the rest of the SDK compiles for any target, so a plugin
// author can unit-test their handler as ordinary Go on their laptop. Only this
// file is WASM-specific.
//
// The pinning below is the part that is easy to get wrong and impossible to
// notice. A Go-compiled guest's collector will reclaim a buffer with no live Go
// reference — including one whose only reference is a raw pointer the host is
// holding. Without the pin map, the host writes a payload, the collector reuses
// the memory, and the guest reads whatever landed there: not a crash, not an
// error, just a subtly wrong request. During development this produced
// `{"echo":"       om the h @"}` from an input of `hello from the host`, which is
// exactly how it would look in production if nobody had checked.

// pinned holds every buffer handed across the boundary, keyed by the pointer the
// host was given. Entries live until the host calls free.
var pinned = map[uint32][]byte{}

//go:wasmexport mcpdoll_alloc
func mcpdollAlloc(size int32) uint32 {
	if size < 0 {
		return 0
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	pinned[ptr] = buf
	return ptr
}

//go:wasmexport mcpdoll_free
func mcpdollFree(ptr uint32) {
	delete(pinned, ptr)
}

//go:wasmexport mcpdoll_invoke
func mcpdollInvoke(ptr uint32, length uint32) uint64 {
	// The input buffer belongs to the host until it frees it, so read rather
	// than retain.
	input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)
	owned := make([]byte, len(input))
	copy(owned, input)

	output := dispatch(owned)

	outPtr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(output))))
	pinned[outPtr] = output
	return uint64(outPtr)<<32 | uint64(len(output))
}
