package health

import (
	"strings"
	"testing"
)

// healthy is the state the apl-overlay asserts: a PVC-backed WAL with replay
// headroom. Every case below is a single deviation from it, so a failure names
// exactly which property regressed.
const wantClass = "block-storage-retain"

func healthyIngester() LokiIngesterSpec {
	return LokiIngesterSpec{
		Namespace:       "monitoring",
		Name:            "loki-ingester-0",
		MemoryLimit:     "3Gi",
		WALVolumeSource: "persistentVolumeClaim",
		WALStorageClass: wantClass,
		WALClassKnown:   true,
		// The ceiling the overlay asserts, and the reason it is part of "healthy"
		// rather than an extra: a 3Gi limit with Loki's 4GB default ceiling is the
		// production crashloop, so a fixture without it would be asserting that
		// the known-broken shape passes.
		WALReplayCeiling: "1536MB",
		WALCeilingKnown:  true,
	}
}

func verdict(t *testing.T, s LokiIngesterSpec) (string, bool) {
	t.Helper()
	msgs, failed := LokiIngesterDurability(s, "3Gi", wantClass)
	return strings.Join(msgs, "\n"), failed
}

// THE PRODUCTION FAILURE, EXACTLY AS OBSERVED: a 1Gi limit on an emptyDir WAL.
// Both halves must be reported, because fixing only one leaves the crashloop.
func TestTheObservedOOMCrashloopFailsOnBothCounts(t *testing.T) {
	s := healthyIngester()
	s.MemoryLimit, s.WALVolumeSource = "1Gi", "emptyDir"
	got, failed := verdict(t, s)
	if !failed {
		t.Fatalf("a 1Gi emptyDir ingester did not fail the lane — the LIMIT half gates, and "+
			"this is the 16-day outage:\n%s", got)
	}
	if !strings.Contains(got, "below the 3Gi WAL-replay floor") {
		t.Errorf("the memory finding is missing:\n%s", got)
	}
	if !strings.Contains(got, "emptyDir") {
		t.Errorf("the volume finding is missing — an operator who fixes only the limit "+
			"gets a still-broken cluster and no second finding to read:\n%s", got)
	}
	// The message must name what IS there, not only what is wanted: "1Gi" is the
	// fact that makes the diagnosis, and its absence sends the reader to a cluster.
	if !strings.Contains(got, "1Gi") {
		t.Errorf("the observed limit is not printed:\n%s", got)
	}
}

// A LIMIT FIX ALONE MUST STILL REPORT THE WAL. This is the arm that would quietly
// disappear if the two properties were ever collapsed into one verdict with an
// early return — an operator whose limit fix did not end the crashloop needs the
// second finding, whether or not it gates.
func TestRaisingTheLimitDoesNotExcuseAnEmptyDirWAL(t *testing.T) {
	s := healthyIngester()
	s.WALVolumeSource = "emptyDir"
	got, _ := verdict(t, s)
	if !strings.Contains(got, "emptyDir") || !strings.Contains(got, "FAIL:") {
		t.Errorf("3Gi on an emptyDir reported no WAL finding — a replay OOM at ANY limit "+
			"re-reads the same WAL forever, so the limit does not make the loop survivable:\n%s", got)
	}
}

func TestTheAssertedConfigurationPasses(t *testing.T) {
	got, failed := verdict(t, healthyIngester())
	if failed {
		t.Errorf("the configuration the overlay asserts was graded a failure:\n%s", got)
	}
}

// A limit ABOVE the floor is fine — the floor is a minimum, not an equality.
// Pinning this stops a future "must equal the asserted value" tightening from
// failing every cluster an operator has deliberately given more headroom.
func TestALimitAboveTheFloorPasses(t *testing.T) {
	s := healthyIngester()
	s.MemoryLimit = "4Gi"
	if got, failed := verdict(t, s); failed {
		t.Errorf("4Gi failed a 3Gi floor:\n%s", got)
	}
}

// FAIL-CLOSED ON "COULD NOT TELL". Each of these is a state where the gate does
// not know the answer, and each must be REPORTED as a failure rather than launder
// an absence of evidence into a green line. Whether it also fails the lane is a
// separate question — see TestTheVolumeHalfReportsWithoutGating.
func TestUnreadableStatesAreReportedAsFailures(t *testing.T) {
	for name, mutate := range map[string]func(*LokiIngesterSpec){
		"unparseable limit":    func(s *LokiIngesterSpec) { s.MemoryLimit = "3 gigabytes" },
		"no WAL volume":        func(s *LokiIngesterSpec) { s.WALVolumeSource = "" },
		"unreadable PVC class": func(s *LokiIngesterSpec) { s.WALClassKnown = false },
		"unreadable config":    func(s *LokiIngesterSpec) { s.WALCeilingKnown = false },
		"unparseable ceiling":  func(s *LokiIngesterSpec) { s.WALReplayCeiling = "lots" },
	} {
		t.Run(name, func(t *testing.T) {
			s := healthyIngester()
			mutate(&s)
			got, _ := verdict(t, s)
			if !strings.Contains(got, "FAIL:") {
				t.Errorf("%s produced no FAIL line — 'could not tell' must not read as "+
					"'nothing wrong':\n%s", name, got)
			}
			// Case-insensitive: one arm emphasises "NOT vouching", the other
			// reads plainly. The property is that the gate SAYS it is declining
			// to vouch, not how loudly.
			if !strings.Contains(strings.ToLower(got), "vouching") {
				t.Errorf("the message does not say the gate is declining to vouch:\n%s", got)
			}
		})
	}
}

// A BROKEN FLOOR MUST NOT PASS EVERYTHING. If the constant this compares against
// ever stops parsing, every verdict it produces is meaningless — and the
// dangerous outcome is not an error, it is a silent all-clear.
func TestAnUnparseableFloorFailsRatherThanPassesEverything(t *testing.T) {
	msgs, failed := LokiIngesterDurability(healthyIngester(), "three gigs", wantClass)
	if !failed {
		t.Errorf("an unparseable floor graded a pass:\n%s", strings.Join(msgs, "\n"))
	}
}

// NO LIMIT IS NOT THE FAILURE THIS CATCHES. An ingester with no memory limit
// cannot be OOMKilled by one. Grading it a failure would push operators to set a
// limit to satisfy a gate — the opposite of the intent — so it passes, with the
// node-pressure caveat stated.
func TestAnAbsentLimitPassesWithItsCaveat(t *testing.T) {
	s := healthyIngester()
	s.MemoryLimit = ""
	got, failed := verdict(t, s)
	if failed {
		t.Errorf("an unlimited ingester was failed:\n%s", got)
	}
	if !strings.Contains(got, "pressure its node") {
		t.Errorf("the caveat is missing, so the pass looks unconditional:\n%s", got)
	}
}

// The remedy must name the key an operator would actually edit. A finding that
// says "raise the limit" without saying where costs a repository search on the
// worst day.
func TestTheFailureNamesTheOverlayKeyToEdit(t *testing.T) {
	s := healthyIngester()
	s.MemoryLimit, s.WALVolumeSource = "1Gi", "emptyDir"
	got, _ := verdict(t, s)
	for _, want := range []string{
		"apps.loki._rawValues.ingester.resources.limits.memory",
		"apps.loki._rawValues.ingester.persistence",
		"apl-values/_shared/apl-overlay/appvalues.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("remedy does not name %q:\n%s", want, got)
		}
	}
}

func TestQuantityBytesSeparatesZeroFromUnparseable(t *testing.T) {
	for in, want := range map[string]struct {
		b  int64
		ok bool
	}{
		"3Gi":     {3 << 30, true},
		"512Mi":   {512 << 20, true},
		"1.5Ti":   {1<<40 + 1<<39, true},
		"1000000": {1000000, true},
		"1G":      {1e9, true},
		"0":       {0, true},
		"":        {0, false},
		"3 gigs":  {0, false},
		"Gi":      {0, false},
	} {
		b, ok := QuantityBytes(in)
		if b != want.b || ok != want.ok {
			t.Errorf("QuantityBytes(%q) = (%d, %v), want (%d, %v)", in, b, ok, want.b, want.ok)
		}
	}
}

// THE BUG A VOLUME-TYPE CHECK CANNOT SEE, and the reason this file grew a
// storage-class field. The first version of the overlay asserted the class under
// `persistence.storageClass`, one level above where the chart reads it
// (`persistence.claims[].storageClass`). Helm accepted the key and ignored it, so
// the PVC was created — at the chart default, with NO storageClassName. "It is a
// PVC" passed. The WAL, which carries the OpenBao audit stream, sat on the
// default provisioner unencrypted.
func TestAPVCWithNoStorageClassIsAFinding(t *testing.T) {
	s := healthyIngester()
	s.WALStorageClass = ""
	got, _ := verdict(t, s)
	if !strings.Contains(got, "NO storageClassName") {
		t.Fatalf("a PVC with no storageClassName was not reported — that is the default "+
			"provisioner holding the audit stream unencrypted:\n%s", got)
	}
	if !strings.Contains(got, "claims[].storageClass") {
		t.Errorf("the remedy does not name the CLAIMS key, which is the whole mistake:\n%s", got)
	}
}

// A PVC on the WRONG class is equally a finding, and the message must print the
// class it actually found — "not the right class" without naming it sends the
// reader to a cluster.
func TestAPVCOnTheWrongStorageClassIsAFinding(t *testing.T) {
	s := healthyIngester()
	s.WALStorageClass = "linode-block-storage"
	got, _ := verdict(t, s)
	if !strings.Contains(got, "FAIL:") {
		t.Fatalf("a WAL on the unencrypted default class was not reported:\n%s", got)
	}
	if !strings.Contains(got, "linode-block-storage") {
		t.Errorf("the finding does not name the class it found:\n%s", got)
	}
}

// AN UNRECOGNISED VOLUME SOURCE IS A FAILURE, and this arm had it backwards: it
// graded anything it did not recognise OK while its own message described the
// non-durability that fails the emptyDir case. `persistence.inMemory: true` is a
// real chart option that lands here.
func TestAnUnrecognisedVolumeSourceIsNotVouchedFor(t *testing.T) {
	s := healthyIngester()
	s.WALVolumeSource = "ephemeral"
	got, _ := verdict(t, s)
	if !strings.Contains(got, "FAIL:") {
		t.Errorf("an unrecognised volume source was graded a pass:\n%s", got)
	}
	if !strings.Contains(got, "ephemeral") {
		t.Errorf("the finding does not name the source it found:\n%s", got)
	}
}

// THE GATING SPLIT. The limit half gates because LLZ can deliver it (resources
// live in the pod template). The volume half does not, because it CANNOT be
// delivered by this channel at all — volumeClaimTemplates is immutable and
// apl-operator creates the StatefulSet before the overlay lands — so gating would
// mean a permanently red lane on every cluster, including every fresh e2e run.
//
// Pinned in both directions, because both are easy to get wrong: a later change
// that gates the volume half silently blocks every promote, and one that stops
// gating the limit half removes the check that catches the actual OOM.
func TestTheVolumeHalfReportsWithoutGating(t *testing.T) {
	onlyVolumeBad := healthyIngester()
	onlyVolumeBad.WALVolumeSource = "emptyDir"
	got, failed := verdict(t, onlyVolumeBad)
	if failed {
		t.Errorf("an emptyDir WAL failed the lane — no cluster can currently satisfy the PVC "+
			"assertion, so this makes assert-loki permanently red everywhere:\n%s", got)
	}
	if !strings.Contains(got, "REPORTED, NOT GATING") {
		t.Errorf("the finding does not say it is not gating, so a reader cannot tell it from "+
			"a check that simply passed:\n%s", got)
	}
	if !strings.Contains(got, "upstream-asks.md") {
		t.Errorf("the finding does not point at the migration that clears it:\n%s", got)
	}

	onlyLimitBad := healthyIngester()
	onlyLimitBad.MemoryLimit = "1Gi"
	if _, failed := verdict(t, onlyLimitBad); !failed {
		t.Error("a below-floor memory limit did NOT fail the lane — that half is deliverable " +
			"and is the one that catches the OOM this whole check is named for")
	}
}

// THE DEFECT THIS PROPERTY WAS ADDED FOR, as measured on a live cluster: the
// limit is correct, the WAL is on the right class, every previously-asserted
// property is green — and the ingester still OOMKills every ~11s forever,
// because no ceiling is set and Loki's 4GB default cannot be reached inside a
// 3Gi cgroup. Before this predicate that cluster was graded HEALTHY.
func TestAnAbsentCeilingFailsEvenWhenEverythingElseIsRight(t *testing.T) {
	s := healthyIngester()
	s.WALReplayCeiling = ""
	got, failed := verdict(t, s)
	if !failed {
		t.Errorf("an ingester with no replay ceiling was graded healthy — this is the exact "+
			"shape that ran 205 restarts over 2d7h with ingestion down:\n%s", got)
	}
	if !strings.Contains(got, "replay_memory_ceiling") {
		t.Errorf("the finding does not name the setting an operator must add:\n%s", got)
	}
}

// A ceiling AT OR ABOVE the limit is the same failure spelled differently: the
// process is killed before Loki ever decides to flush. Pinned separately from
// the absent case because the remedy differs — one adds a key, the other
// corrects a number — and because "it is set" is exactly the reasoning that
// would wave this through.
func TestACeilingThatDoesNotFitInsideTheLimitFails(t *testing.T) {
	for _, ceiling := range []string{"3Gi", "4GB", "8GiB"} {
		s := healthyIngester()
		s.WALReplayCeiling = ceiling
		if got, failed := verdict(t, s); !failed {
			t.Errorf("ceiling %s passed against a 3Gi limit, but it can never be reached:\n%s",
				ceiling, got)
		}
	}
}

// CONTROL for the above: the grammars really are different, and this is the
// assertion that would have caught reusing QuantityBytes for both. "1536MB" is
// ~1.54e9 bytes and fits inside 3Gi (~3.22e9); QuantityBytes cannot parse it at
// all, so a single-parser implementation fails this test rather than shipping a
// gate that is red on every correct cluster.
func TestTheAssertedCeilingSpellingParsesAndFits(t *testing.T) {
	if _, ok := QuantityBytes("1536MB"); ok {
		t.Errorf("QuantityBytes now parses a humanize spelling; if that is deliberate, this " +
			"test and LokiByteSize's rationale both need revisiting")
	}
	got, ok := LokiByteSize("1536MB")
	if !ok {
		t.Fatalf("LokiByteSize could not parse the spelling the overlay ships")
	}
	limit, _ := QuantityBytes("3Gi")
	if got >= limit {
		t.Errorf("the asserted ceiling %d is not below the asserted limit %d", got, limit)
	}
}

// Both grammars, including the ones that differ only by a trailing B. A suffix
// table ordered wrong reads "1536MiB" as 1536 bytes and passes every comparison
// for the wrong reason.
func TestLokiByteSizeReadsBothGrammars(t *testing.T) {
	for in, want := range map[string]int64{
		"1536MB":     1536e6,
		"1536MiB":    1536 << 20,
		"1.5GB":      1.5e9,
		"4GB":        4e9,
		"1073741824": 1 << 30,
		"512Mi":      512 << 20,
	} {
		got, ok := LokiByteSize(in)
		if !ok || got != want {
			t.Errorf("LokiByteSize(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "lots", "3 gigabytes", "-1GB"} {
		if got, ok := LokiByteSize(bad); ok {
			t.Errorf("LokiByteSize(%q) = %d, true; want a refusal — a folded zero compares "+
				"as below every limit and manufactures a pass", bad, got)
		}
	}
}
