package main

// kube_exec_deps.go — points internal/kube's Exec seam at package main's.
//
// The Secret probes moved to internal/kube with their own seam so they are
// testable without a cluster; this keeps the ONE shell-out implementation in
// package main rather than letting two drift.

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kube"

// A DELEGATING CLOSURE, NOT `kube.Exec = execOutput`.
//
// The direct assignment snapshots whatever execOutput points at WHEN init RUNS —
// before any test swaps it — so every test that stubs execOutput would still reach
// the real kubectl through this package. That is the capture bug this campaign
// already paid for once, with harborCARetrofitKubectl. The closure reads the
// variable at call time, which is the whole point of a seam.
func init() {
	kube.Exec = func(name string, args ...string) ([]byte, error) { return execOutput(name, args...) }
}
