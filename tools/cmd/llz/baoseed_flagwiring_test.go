package main

// baoseed_flagwiring_test.go — the bao-seed flag-set test, which stayed.
//
// It walks the LIVE cobra tree, so only package main can build it — the same call
// docsguard's six and manifestguard's one made. It travelled to internal/kube by
// accident because its body mentions DescribeSecret; the classifier's one blind
// spot, already recorded with assert-identity.

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/openbao"
)

// stubBaoSeedKV stubs baoread.ExecFn for bao-seed runs: `kv get` of presentPath/
// presentField returns presentValue; every `kv put` is recorded.
func TestBaoSeedCmdFlagWiring(t *testing.T) {
	c := openbao.BaoSeedCmd()
	for _, f := range []string{"path", "field", "skip-if-present", "on-missing",
		"on-missing-standby", "missing-note", "missing-note-standby",
		"missing-annotation", "summary-on-seed", "seeded-message"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("bao-seed must define --%s", f)
		}
	}
	if got := c.Flags().Lookup("on-missing").DefValue; got != "error" {
		t.Errorf("--on-missing default = %q, want error", got)
	}
}

// TestDescribeSecretForDiagNeverLeaksValues is the load-bearing property. This
// helper exists to be printed into a CI log on a failure path, so it must report
// key NAMES and never key CONTENTS. Asserting on the jsonpath it asks kubectl for
// is what actually pins that: `{$k}` iterates keys, and any expression reaching
// `$v` (or a `.data.<key>` selector) would put decoded secret material in the log.
// TestDescribeSecretForDiagNeverLeaksValues is the load-bearing property. This
// helper exists to be printed into a CI log on a failure path, so it must report
// key NAMES and never key CONTENTS. Asserting on the jsonpath it asks kubectl for
// is what actually pins that: `{$k}` iterates keys, and any expression reaching
// `$v` (or a `.data.<key>` selector) would put decoded secret material in the log.
