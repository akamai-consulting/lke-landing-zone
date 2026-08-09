package main

// render.LineDiff is in render.go; boolPtrLocal is a package main test helper. Both were
// passengers in env_set_test.go.

func boolPtrLocal(b bool) *bool { return &b }

// #9: the LCS diff shows scattered changes as separate hunks with a collapse marker.
