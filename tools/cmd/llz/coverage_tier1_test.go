package main

// Tier-1 coverage: table tests for the small PURE helpers that carried no direct
// test (they were only exercised incidentally through larger orchestrators).
// Each is deterministic on its inputs — no kubectl / API / filesystem.
