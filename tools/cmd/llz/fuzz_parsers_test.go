package main

// Fuzz targets for the string parsers in cmd/llz.
//
// Why fuzzing, and why these functions: mutation testing surfaced two defects in
// this package that no hand-written fixture would have reached. The container
// image-reference helpers PANICKED on a bare name (both LastIndex(":") and
// LastIndex("/") return -1, and a mutated bound then sliced [:-1]) — found only
// because a mutant happened to walk into it. And in internal/linode the
// days-from-civil `doe/146_096` term is non-zero on exactly ONE day per 400
// years, so it stayed invisible until an exhaustive sweep. Both are input-space
// problems, which is what fuzzing is for and what example-based tests are not.
//
// These targets assert INVARIANTS rather than expected outputs, because the
// interesting inputs are the ones nobody would think to write down:
//
//   - the function does not panic
//   - it is deterministic (same input, same answer)
//   - its output satisfies the contract stated in its doc comment
//
// Seed corpora below are the shapes that actually occur (registry ports, digests,
// bare names) plus the degenerate ends. `go test` runs the seed corpus as ordinary
// subtests at zero cost, so these act as regression tests even when nobody is
// fuzzing; `make fuzz` explores beyond them. Any crasher found gets written to
// testdata/fuzz/ by the toolchain and then runs forever as part of the corpus.

// optional digest
