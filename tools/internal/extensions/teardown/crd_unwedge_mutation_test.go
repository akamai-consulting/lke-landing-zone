package teardown

import "testing"

// crdUnwedgeThreshold is the point at which the annotation is stripped, so an
// annotation of EXACTLY that many bytes is already over budget and must be
// stripped. Treating the boundary as "still fine" leaves the CRD one byte of
// growth away from the 256KB apiserver limit that this unwedge exists to keep
// clear of.
func TestStripOversizedCRDLastAppliedStripsAtExactlyTheThreshold(t *testing.T) {
	body := crdListJSON(t, map[string]int{"exact.example.com": crdUnwedgeThreshold})
	var annotated []string
	got := StripOversizedCRDLastApplied(func(args ...string) (string, bool) {
		if args[0] == "get" {
			return body, true
		}
		annotated = append(annotated, args[2])
		return "", true
	})
	if len(got) != 1 || got[0] != "exact.example.com" {
		t.Errorf("an annotation of exactly %d bytes must be stripped, got %v", crdUnwedgeThreshold, got)
	}
	if len(annotated) != 1 {
		t.Errorf("kubectl annotate calls = %v, want one", annotated)
	}
}
