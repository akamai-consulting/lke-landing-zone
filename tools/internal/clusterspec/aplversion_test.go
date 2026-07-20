package clusterspec

import (
	"strings"
	"testing"
)

func TestAplSemver(t *testing.T) {
	cases := []struct {
		in              string
		maj, min, patch int
		ok              bool
	}{
		{"6.0.0", 6, 0, 0, true},
		{"v6.1.2", 6, 1, 2, true},
		{"6.1.0-rc.1", 6, 1, 0, true}, // pre-release suffix ignored
		{" 5.0.0 ", 5, 0, 0, true},
		{"6.0", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"latest", 0, 0, 0, false},
		{"6.x.0", 0, 0, 0, false},
	}
	for _, c := range cases {
		maj, min, patch, ok := aplSemver(c.in)
		if ok != c.ok || (ok && (maj != c.maj || min != c.min || patch != c.patch)) {
			t.Errorf("aplSemver(%q) = %d.%d.%d ok=%v, want %d.%d.%d ok=%v",
				c.in, maj, min, patch, ok, c.maj, c.min, c.patch, c.ok)
		}
	}
}

func TestEffectiveAplChartVersion(t *testing.T) {
	if got := EffectiveAplChartVersion(""); got != BaselineAplChartVersion {
		t.Errorf("empty pin = %q, want the baseline %q", got, BaselineAplChartVersion)
	}
	if got := EffectiveAplChartVersion("5.0.0"); got != "5.0.0" {
		t.Errorf("explicit pin = %q, want it preserved", got)
	}
}

func TestAplChartDriftOf(t *testing.T) {
	// Anchored on a 6.x baseline; update alongside BaselineAplChartVersion.
	cases := map[string]AplChartDrift{
		"":           AplChartDriftNone,
		"6.0.0":      AplChartDriftNone,
		"5.0.0":      AplChartDriftMajorBehind,
		"4.9.9":      AplChartDriftMajorBehind,
		"7.0.0":      AplChartDriftMajorAhead,
		"6.1.0":      AplChartDriftMinor,
		"6.0.1":      AplChartDriftMinor,
		"not-semver": AplChartDriftUnparseable,
	}
	for pin, want := range cases {
		if got := AplChartDriftOf(pin); got != want {
			t.Errorf("AplChartDriftOf(%q) = %v, want %v", pin, got, want)
		}
	}
}

// The regression this whole gate exists for: an instance pinned to APL 5 that
// upgrades llz to the APL 6 release must FAIL validation rather than silently
// keep deploying 5.
func TestAplChartVersionError_MajorBehindBlocks(t *testing.T) {
	err := aplChartVersionError("prod", "5.0.0")
	if err == nil {
		t.Fatal("a major-behind pin must be a blocking spec error")
	}
	for _, want := range []string{"prod", "5.0.0", BaselineAplChartVersion, AllowMajorDriftEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestAplChartVersionError_AllowsBaselineAndMinorDrift(t *testing.T) {
	for _, pin := range []string{"", BaselineAplChartVersion, "6.1.0"} {
		if err := aplChartVersionError("prod", pin); err != nil {
			t.Errorf("pin %q must not block, got: %v", pin, err)
		}
	}
}

func TestAplChartVersionError_MajorAheadBlocks(t *testing.T) {
	if err := aplChartVersionError("dev", "7.0.0"); err == nil {
		t.Error("a major-ahead pin must block — llz is untested against it")
	}
}

func TestAplChartVersionError_UnparseableBlocks(t *testing.T) {
	if err := aplChartVersionError("dev", "latest"); err == nil {
		t.Error("an unparseable pin must block")
	}
}

// The escape hatch releases the major gate but must NOT swallow a malformed pin.
func TestAplChartVersionError_EscapeHatch(t *testing.T) {
	t.Setenv(AllowMajorDriftEnv, "1")
	if err := aplChartVersionError("prod", "5.0.0"); err != nil {
		t.Errorf("%s=1 must permit staged major drift, got: %v", AllowMajorDriftEnv, err)
	}
	if err := aplChartVersionError("prod", "nonsense"); err == nil {
		t.Error("the escape hatch must not excuse an unparseable pin")
	}
}

func TestAplChartVersionWarnings(t *testing.T) {
	lz := &LandingZone{Spec: Spec{Environments: map[string]Environment{
		"dev":  {Cluster: Cluster{Bootstrap: Bootstrap{AplChartVersion: "6.1.0"}}},
		"prod": {Cluster: Cluster{Bootstrap: Bootstrap{AplChartVersion: BaselineAplChartVersion}}},
	}}}
	ws := lz.AplChartVersionWarnings()
	if len(ws) != 1 || !strings.Contains(ws[0], "dev") {
		t.Errorf("want exactly one warning naming dev, got %v", ws)
	}
}
