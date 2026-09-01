package clusterspec

// overlay_scalar_equal_test.go pins how a delivered scalar is compared to a
// declared one.
//
// THREE READERS, AND ONE OF THEM DELETES THINGS. OverlayScalarEqual is reached
// through OverlayFieldDelivered from `llz ci assert-overlay-applied`, from the
// appliability probe, and from the BROWNFIELD MIGRATION PRECONDITION — "is this
// field still undelivered, so is there anything to migrate?". A false mismatch
// there answers yes forever and orphan-deletes a live StatefulSet on every
// platform-scope run; a false match hides real drift. So both directions are
// pinned, and the table is written against what a real apiserver does rather than
// against what the implementation happens to accept.
//
// THE TWO FAILURES THIS TABLE EXISTS FOR, both found by attacking the first
// version of the function:
//
//	the apiserver CANONICALISES   patch 3072Mi and read back 3Gi, patch 1000m and
//	                              read back 1, patch 1.5m and read back 1500u. A
//	                              text compare calls a delivered value undelivered.
//	a number is not a quantity    taking the numeric path for anything that parses
//	                              made "2.10" == "2.1" and "0755" == "755" — so a
//	                              chart version, an image tag or a file mode would
//	                              compare NUMERICALLY. Latent, because no declared
//	                              value is shaped that way today.

import "testing"

func TestOverlayScalarEqualAgreesWithWhatAnApiserverWouldDo(t *testing.T) {
	for _, c := range []struct {
		a, b any
		want bool
		why  string
	}{
		// Canonicalisation: the same value, spelled the way each side spells it.
		{"3Gi", "3072Mi", true, "binary rescale"},
		{"1", "1000m", true, "milli to whole"},
		{"1Gi", "1073741824", true, "suffix versus plain integer"},
		{"1500u", "1.5m", true, "sub-milli, the apiserver's own canonical output"},
		{"100n", "0.0000001", true, "nano"},
		{"3e9", "3000000000", true, "exponent form, which the apiserver PRESERVES"},
		{"1.5e2", "150", true, "decimal exponent"},
		{"1e3", "1k", true, "exponent and suffix together"},
		{"+1", "1000m", true, "leading sign"},
		{"-1", "-1000m", true, "negative"},

		// THE TYPES A DECODED LIVE OBJECT ACTUALLY CARRIES, not the strings every other
		// row here feeds. `json.Unmarshal` into map[string]any yields float64 for an
		// unquoted numeric leaf, and the int/int64 arms exist for values built in Go
		// rather than decoded. All three arms of quantityRat's type switch could be made
		// to report "not a quantity" with 146 packages green — and the failure that
		// buys is a false MISMATCH, which the migration precondition turns into a
		// recreate with no terminating condition.
		{float64(1073741824), "1Gi", true, "float64 from a decoded object versus a suffixed declaration"},
		{int64(1073741824), "1Gi", true, "int64 versus a suffixed declaration"},
		{1073741824, "1Gi", true, "int versus a suffixed declaration"},
		{int64(1), "1000m", true, "int64 whole versus milli"},
		{float64(0.5), "500m", true, "float64 fraction versus milli"},
		{float64(2147483648), "1Gi", false, "a float64 that is genuinely a different quantity"},

		// Genuinely different quantities.
		{"1Gi", "2Gi", false, "different"},
		{"1Gi", "1G", false, "binary and decimal are not the same number"},

		// NOT quantities. Each of these compared EQUAL under a numeric-only rule,
		// and each is a shape a future overlay row could plausibly declare.
		{"2.10", "2.1", false, "a chart version"},
		{"6.20", "6.2", false, "a chart version"},
		{"1.0", "1", false, "a bare decimal"},
		{"01", "1", false, "a leading zero"},
		{"0755", "755", false, "a file mode"},

		// Whitespace. resource.Quantity rejects it outright, so accepting it here
		// would vouch for a declaration the apiserver will refuse as malformed.
		{" 3Gi", "3Gi", false, "leading space"},
		{"3Gi\n", "3Gi", false, "trailing newline"},

		// Everything else is text, exactly as before.
		{"restricted", "restricted", true, "plain string"},
		{"1K", "1k", false, "K is not a Kubernetes suffix; kilo is lowercase"},
		{nil, "", false, "nil is not a value"},
	} {
		if got := OverlayScalarEqual(c.a, c.b); got != c.want {
			t.Errorf("OverlayScalarEqual(%v, %v) = %v, want %v — %s", c.a, c.b, got, c.want, c.why)
		}
	}
}

func TestTodaysDeclaredValuesAreUnaffectedByTheQuantityPath(t *testing.T) {
	// A behaviour change on a value the overlay actually declares would be a silent
	// regression in three lanes. Every scalar row's Prior must still differ from its
	// declared value exactly where it did before — that difference is what decides
	// whether the row is probed at all.
	raw := AplAppRawValues()
	checked := 0
	for _, f := range OverlayFields() {
		if f.Match != MatchScalar || f.Prior == nil {
			continue
		}
		declared, ok := RawValue(raw[f.App], f.Value...)
		if !ok {
			t.Errorf("%s declares no value", OverlayFieldPath(f))
			continue
		}
		// The text compare is the pre-change behaviour. For today's values the two
		// must agree; the day they do not, someone has spelled a quantity
		// non-canonically and should learn it here rather than from a recreate.
		text := OverlayFieldPath(f) != "" && (declared == f.Prior)
		if OverlayScalarEqual(f.Prior, declared) != text {
			t.Errorf("%s: the quantity path changes this row's verdict (Prior %v, declared %v). "+
				"That decides whether the row is probed, and in the migration precondition "+
				"whether a live object is deleted", OverlayFieldPath(f), f.Prior, declared)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no scalar row was checked — this test proved nothing")
	}
}

func TestBothHalvesSpellANumberTheSameWay(t *testing.T) {
	// quantityRat normalises a float64 with FormatFloat('f',-1,64); the text
	// fallback used %v, which switches to exponent form around 1e7. So a live JSON
	// NUMBER at or above that could never equal an unsuffixed declared value —
	// "3e+09" against "3000000000" — and in the migration precondition "still
	// undelivered" answers yes forever, which is a live StatefulSet recreated on
	// every platform-scope run with no terminating condition.
	for _, c := range []struct {
		live, declared any
		want           bool
		why            string
	}{
		{float64(3000000000), "3000000000", true, "a large JSON number against its plain spelling"},
		{float64(1e7), "10000000", true, "the threshold %v switches at"},
		{float64(3000000000), "3000000001", false, "genuinely different"},
		{float64(1), "1", true, "small numbers were never affected"},
		{float64(1.5), "1.5", true, "decimals"},
		{3000000000, "3000000000", true, "an int, for the same reason"},
	} {
		if got := OverlayScalarEqual(c.live, c.declared); got != c.want {
			t.Errorf("OverlayScalarEqual(%v, %v) = %v, want %v — %s", c.live, c.declared, got, c.want, c.why)
		}
	}
}
