package assertplatform

// verdict_test.go — THE GATE FOR ISSUE #449.
//
// The behaviour being gated is not "the preflight decides correctly" (k8s_version_test.go
// covers that). It is that the preflight SAYS what it decided, on every path, in a
// form something other than a human reading a step log can count. The failure this
// protects against is silence: a pipeline where the catalog read always fails, so
// every run of this gate is green having checked nothing, and no artifact anywhere
// distinguishes that from a run that verified the pin.
//
// So the assertions are made on the DURABLE OUTPUT — what lands in
// $GITHUB_STEP_SUMMARY — rather than on the return value of a helper. A record
// that exists only inside the process is exactly the thing that was already there.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// summaryOf runs fn with $GITHUB_STEP_SUMMARY pointed at a fresh file and returns
// what was written to it.
func summaryOf(t *testing.T, fn func()) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", path)
	fn()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	return string(b)
}

// TestEveryOutcomeOfThePreflightRecordsWhatItDecided walks every arm the verb can
// return through and requires each to leave exactly one verdict record naming its
// outcome.
//
// EXACTLY ONE, NOT AT LEAST ONE. "At least one" passes on a run that recorded a
// soft pass and then recorded a decision as well, which is a record that can be
// read either way and therefore says nothing.
//
// THE ARMS THAT MATTER MOST ARE THE PASSING ONES. A failing arm is never silent —
// the build stops. It is the three UNDECIDED arms that exit 0, look identical to a
// real pass in the exit status, and are the whole reason this file exists.
func TestEveryOutcomeOfThePreflightRecordsWhatItDecided(t *testing.T) {
	for _, tc := range []struct {
		name        string
		env         string
		setup       func(t *testing.T)
		wantKind    k8sVerdictKind
		wantDecided string
		wantReason  string
	}{
		{
			name: "the account offers the pin",
			env:  "prod",
			setup: func(t *testing.T) {
				withSpec(t, specPinning("prod", "v1.34.6+lke2"), true, nil)
				withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
			},
			wantKind: k8sOffered, wantDecided: "yes",
		},
		{
			name: "the account does not offer it and no cluster runs it",
			env:  "prod",
			setup: func(t *testing.T) {
				withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
				withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
			},
			wantKind: k8sNotOffered, wantDecided: "yes",
		},
		{
			name: "not offered, but this deployment's cluster already runs it",
			env:  "prod",
			setup: func(t *testing.T) {
				withSpec(t, specPinning("prod", "v1.33.6+lke7"), true, nil)
				withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
				withClusters(t, func(context.Context) ([]map[string]any, error) {
					return []map[string]any{lkeCluster("llz-prod", "us-ord", "v1.33.6+lke7")}, nil
				})
			},
			wantKind: k8sExempt, wantDecided: "yes",
		},
		{
			// THE #449 ARM. This is the one that 401s in release-e2e, exits 0, and used
			// to be indistinguishable from the first case above.
			name: "the catalog could not be read",
			env:  "prod",
			setup: func(t *testing.T) {
				withSpec(t, specPinning("prod", "v1.34.6+lke2"), true, nil)
				withLister(t, func(context.Context) ([]string, error) {
					return nil, errors.New("GET /v4beta/lke/tiers/enterprise/versions returned 401: Invalid Token")
				})
			},
			wantKind: k8sUndecided, wantDecided: "no", wantReason: reasonCatalogUnreadable,
		},
		{
			name: "the account reported no versions at all",
			env:  "prod",
			setup: func(t *testing.T) {
				withSpec(t, specPinning("prod", "v1.34.6+lke2"), true, nil)
				withLister(t, func(context.Context) ([]string, error) { return nil, nil })
			},
			wantKind: k8sUndecided, wantDecided: "no", wantReason: reasonCatalogEmpty,
		},
		{
			name: "the catalog is too coarse to settle a build id",
			env:  "prod",
			setup: func(t *testing.T) {
				withSpec(t, specPinning("prod", "v1.34.6+lke2"), true, nil)
				withLister(t, func(context.Context) ([]string, error) { return []string{"1.34", "1.32"}, nil })
			},
			wantKind: k8sUndecided, wantDecided: "no", wantReason: reasonCatalogCoarse,
		},
		{
			name: "no spec on disk — the template repo",
			env:  "prod",
			setup: func(t *testing.T) {
				withSpec(t, nil, false, nil)
				withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
			},
			wantKind: k8sNoSpec, wantDecided: "no", wantReason: reasonNoSpec,
		},
		{
			name: "--env names a deployment the spec does not define",
			env:  "prd",
			setup: func(t *testing.T) {
				withSpec(t, specPinning("prod", "v1.34.6+lke2"), true, nil)
				withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
			},
			wantKind: k8sSpecRejected, wantDecided: "no", wantReason: reasonSpecUnusable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			out := summaryOf(t, func() { _ = assertK8sVersion(tc.env) })

			var records []string
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, "k8sVersion preflight") {
					records = append(records, l)
				}
			}
			if len(records) != 1 {
				t.Fatalf("wrote %d verdict records, want exactly 1 — a run that records none is "+
					"indistinguishable from one that never ran, and one that records twice can be read "+
					"either way. Summary was:\n%s", len(records), out)
			}
			rec := records[0]
			if !strings.Contains(rec, "`"+string(tc.wantKind)+"`") {
				t.Errorf("record does not name the outcome %q:\n%s", tc.wantKind, rec)
			}
			wantState := "**decided**"
			if tc.wantDecided == "no" {
				wantState = "**UNDECIDED**"
			}
			if !strings.Contains(rec, wantState) {
				t.Errorf("record must say %s — that word is the whole measurement:\n%s", wantState, rec)
			}
			if tc.wantReason != "" && !strings.Contains(rec, tc.wantReason) {
				t.Errorf("record must carry the reason %q, or \"it could not be answered\" is not "+
					"actionable:\n%s", tc.wantReason, rec)
			}
			// The pin travels with the verdict. Without it, `decided=yes` in two runs a
			// week apart cannot be compared: the second may be about a version the first
			// would have rejected.
			if tc.wantKind != k8sNoSpec && !strings.Contains(rec, "prod") && !strings.Contains(rec, "prd") {
				t.Errorf("record must name the deployment it judged:\n%s", rec)
			}
		})
	}
}

// TestTheRecordCarriesThePinItJudged — a decided verdict with no version in it is
// a claim that cannot be checked against anything later.
func TestTheRecordCarriesThePinItJudged(t *testing.T) {
	withSpec(t, specPinning("prod", "v1.34.6+lke2"), true, nil)
	withLister(t, func(context.Context) ([]string, error) { return theE2EAccount, nil })
	out := summaryOf(t, func() { _ = assertK8sVersion("prod") })
	if !strings.Contains(out, "v1.34.6+lke2") {
		t.Errorf("the record must name the pin it decided about; got:\n%s", out)
	}
}

// TestAnUnrecordedOutcomeAnnouncesItself is the fail-closed arm, and it is about a
// future edit rather than today's code: an arm added later that returns without
// setting a verdict must not render as a quiet, plausible-looking record. The zero
// value is a bug, and it says so.
func TestAnUnrecordedOutcomeAnnouncesItself(t *testing.T) {
	v := k8sVerdict{env: "prod", pin: "v1.34.6+lke2"}
	if v.kind.decided() {
		t.Fatal("the zero-value kind must never read as a decision")
	}
	for _, s := range []string{v.record(), v.summary()} {
		if !strings.Contains(s, "UNRECORDED") {
			t.Errorf("an unset verdict must name itself as unrecorded, not render as an outcome; got: %s", s)
		}
	}
}

// TestDecidedMeansTheAccountAnswered pins the narrow definition. Widening it to
// "the verb produced some outcome" would make no-spec and spec-rejected runs count
// as decisions — and a cycle of those is exactly the silence being measured.
func TestDecidedMeansTheAccountAnswered(t *testing.T) {
	for _, k := range []k8sVerdictKind{k8sOffered, k8sNotOffered, k8sExempt} {
		if !k.decided() {
			t.Errorf("%q is the account settling the question; it must count as decided", k)
		}
	}
	for _, k := range []k8sVerdictKind{k8sUndecided, k8sNoSpec, k8sSpecRejected, k8sUnrecorded} {
		if k.decided() {
			t.Errorf("%q is not the account answering — counting it as decided makes the "+
				"measurement agree with the silence it exists to detect", k)
		}
	}
}
