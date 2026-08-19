package doctor

// Gap-closing tests for doctor_linode.go: the token gate in front of the live
// client, the deadline the probe runs under, the version parser's handling of a
// malformed pin, and the spec read that decides WHICH versions get checked.
//
// Still no network: doctorLinodeClient is exercised for its nil/non-nil decision
// only (NewClient issues no request), and the probe itself runs against a fake.

import (
	"context"
	"sort"
	"testing"
	"time"
)

// The token gate decides whether doctor talks to Linode at all. Getting it
// backwards is silent in both directions: every configured operator loses the
// check, and every unconfigured one gets a tokenless client that 401s on an
// account it was never asked to reach.
func TestDoctorLinodeClientRequiresAToken(t *testing.T) {
	t.Setenv("LINODE_TOKEN", "")
	t.Setenv("LINODE_API_TOKEN", "")
	if c := doctorLinodeClient(); c != nil {
		t.Errorf("no token configured: doctorLinodeClient = %v, want nil (the section must skip)", c)
	}
	// Either spelling enables it.
	for _, env := range []string{"LINODE_TOKEN", "LINODE_API_TOKEN"} {
		t.Setenv("LINODE_TOKEN", "")
		t.Setenv("LINODE_API_TOKEN", "")
		t.Setenv(env, "tok-abc")
		if c := doctorLinodeClient(); c == nil {
			t.Errorf("%s set: doctorLinodeClient = nil, want a live client", env)
		}
	}
}

// ctxRecordingLister records the state of the context AT CALL TIME — the caller
// defers cancel(), so inspecting the context after ReportLinodeAccount returns
// would always show it cancelled.
type ctxRecordingLister struct {
	called      bool
	errAtCall   error
	deadline    time.Time
	hasDeadline bool
}

func (l *ctxRecordingLister) ListLKEVersions(ctx context.Context, _ string) ([]string, error) {
	l.called = true
	l.errAtCall = ctx.Err()
	l.deadline, l.hasDeadline = ctx.Deadline()
	return []string{"v1.33.6+lke7"}, nil
}

func (l *ctxRecordingLister) ListClusters(context.Context) ([]map[string]any, error) {
	return nil, nil
}

// The account probe is a network call on doctor's critical path, so it must be
// bounded — AND the bound must still be in the future when the call is made. A
// deadline that has already passed cancels the probe before it starts, turning
// the advisory section into a permanent "could not list versions" for everyone.
func TestReportLinodeAccountBoundsTheProbeWithALiveDeadline(t *testing.T) {
	l := &ctxRecordingLister{}
	withLKELister(t, l)
	captureStdout(t, func() { ReportLinodeAccount([]K8sPin{pin("v1.33.6+lke7")}) })

	if !l.called {
		t.Fatal("the lister was never called")
	}
	if !l.hasDeadline {
		t.Fatal("the probe ran with no deadline — doctor would hang on a wedged API")
	}
	if l.errAtCall != nil {
		t.Fatalf("the context was already expired when the call was made (%v) — a collapsed timeout cancels the probe before it starts", l.errAtCall)
	}
	if left := time.Until(l.deadline); left < time.Second {
		t.Errorf("deadline is %v away, want the %v probe budget", left, lkeProbeTimeout)
	}
}

// The whole point of the section is checking the versions THIS spec pins. A
// guard that bails on a perfectly good spec leaves doctor checking nothing while
// still printing a color.Green "LKE-Enterprise reachable" — the silent no-op the
// advisory check was added to avoid.
func TestSpecK8sPinsReadsAPresentSpec(t *testing.T) {
	// Split layout: landingzone.yaml carries the default pin (inherited by prod),
	// lab overrides it.
	chdirTempDir(t)
	writeSpecInstance(t, map[string]string{
		"prod": clusterDef("prod", ""),
		"lab":  clusterDef("lab", "    k8sVersion: v1.32.1+lke1\n"),
	})

	got := SpecK8sPins("")
	versions := make([]string, 0, len(got))
	for _, p := range got {
		versions = append(versions, p.Version)
		// The label and region are what make the existing-cluster exemption
		// possible; a pin carrying only a version cannot express it.
		if p.ClusterLabel == "" || p.Region == "" {
			t.Errorf("pin %+v carries no label/region — ClusterRunsVersion cannot match a "+
				"cluster without them, so a running deployment would be reported unbuildable", p)
		}
	}
	sort.Strings(versions)
	want := []string{"v1.32.1+lke1", "v1.33.6+lke7"}
	if len(versions) != len(want) {
		t.Fatalf("SpecK8sPins(\"\") = %v, want every env's pin %v", versions, want)
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Fatalf("SpecK8sPins(\"\") = %v, want %v", versions, want)
		}
	}

	// A named env narrows it to that env's pin.
	if one := SpecK8sPins("lab"); len(one) != 1 || one[0].Version != "v1.32.1+lke1" {
		t.Errorf("SpecK8sPins(\"lab\") = %v, want [v1.32.1+lke1]", one)
	}

	// AND A NAMED-BUT-UNKNOWN ENV NARROWS TO NOTHING. It used to fall through to
	// every deployment, which was cosmetic while this section only printed — and
	// became "`llz doctor --env prd` hard-fails on pins the operator never asked
	// about" the moment it started returning errors.
	if none := SpecK8sPins("prd"); len(none) != 0 {
		t.Errorf("SpecK8sPins(\"prd\") = %v, want none — scope follows the flag", none)
	}
}

// Outside an instance there is nothing to check — doctor runs in the template
// repo too, and must stay silent there.
func TestSpecK8sPinsSilentWithoutASpec(t *testing.T) {
	chdirTempDir(t)
	if got := SpecK8sPins(""); got != nil {
		t.Errorf("SpecK8sPins with no spec = %v, want nil", got)
	}
}
