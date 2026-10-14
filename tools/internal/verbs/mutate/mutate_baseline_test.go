package mutate

import "testing"

// The shipped baseline must load with the shipped loader. A baseline that does
// not parse silently degrades the gate to "no baseline", i.e. every survivor new.
func TestShippedBaselineLoads(t *testing.T) {
	got, err := loadBaseline("testdata/mutation-baseline.json")
	if err != nil {
		t.Fatalf("shipped baseline does not load: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("shipped baseline parsed to zero entries")
	}
	const canaryKey = "ci_rotate_dbadmin.go:262:58:ARITHMETIC_BASE"
	if got[canaryKey] == "" {
		t.Errorf("the canary must be in the baseline, keys: %v", got)
	}
	for k, why := range got {
		if len(why) < 40 {
			t.Errorf("%s: baseline entry needs a checkable justification, got %q", k, why)
		}
	}
}
