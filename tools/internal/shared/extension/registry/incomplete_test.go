package registry

// incomplete_test.go — the ratchet on extensions that declare themselves partial.
//
// ────────────────────────────────────────────────────────────────────────────
// SIXTEEN ACKNOWLEDGED HOLES, MEASURED BY NOTHING.
//
// `Incomplete` exists because two extractions arrived partial and the model could
// not say so: reconcile-actions declared four invariants while more of its lanes
// were still elsewhere, template-sustain declared the half that does not touch
// .template-manifest. Both read as COMPLETE, and "an extension that silently
// under-declares its own surface is the same failure shape as PR #15's
// ban-by-omission: the reader cannot tell what is missing."
//
// The field fixed the reader's problem and created a second one. The only rule on
// it was that an entry may not be blank, so nothing counted the notes, nothing
// stopped a thirteenth extension acquiring one, and — the direction that actually
// rots — nothing failed when a hole was FILLED and its note left behind. A stale
// note claims work that is already done, which is exactly how the prose version of
// undrivenGates came to describe a system that had moved on.
//
// So the notes become data, ratcheted in both directions, like undrivenGates,
// censusExempt, batteryExempt, invariantExempt and unclaimedCommands before them.
// The number here is the honest size of "what the registry knows it does not
// declare", and it can only go down without an argument in the diff.
// ────────────────────────────────────────────────────────────────────────────

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// incompleteNotes is how many Incomplete entries each extension carries.
//
// EDITING THIS IS THE POINT. Filling a hole means deleting the note AND its line
// here, in the same commit — which is what banks the paydown instead of leaving
// slack the next partial extraction grows into. Adding a line is legal and is a
// line in a diff a reviewer sees.
var incompleteNotes = map[string]int{
	"bootstrap-cluster":           1,
	"credential-state-passphrase": 1,
	"import-brownfield":           1,
	"obj-encryption":              1,
	"openbao":                     2,
	"pin-coherence":               1,
	"reconcile-actions":           3,
	"release-publish":             1,
	"seed-special":                1,
	"template-commit":             1,
	"template-sustain":            2,
}

func TestIncompleteNotesAreRatcheted(t *testing.T) {
	live := map[string]int{}
	for _, e := range All() {
		if n := len(e.Incomplete); n > 0 {
			live[e.Name] = n
		}
	}

	var appeared, banked, changed []string
	for name, n := range live {
		want, ok := incompleteNotes[name]
		switch {
		case !ok:
			appeared = append(appeared, fmt.Sprintf("%s (%d note(s))", name, n))
		case want != n:
			changed = append(changed, fmt.Sprintf("%s: %d note(s), this table says %d", name, n, want))
		}
	}
	for name, want := range incompleteNotes {
		if _, ok := live[name]; !ok {
			banked = append(banked, fmt.Sprintf("%s (was %d)", name, want))
		}
	}
	sort.Strings(appeared)
	sort.Strings(banked)
	sort.Strings(changed)

	if len(appeared) > 0 {
		t.Errorf("%d extension(s) newly declare themselves partial:\n\t%s\n"+
			"\tThat is allowed — a partial declaration is better than a silent one — but it "+
			"is a claim the extension does not describe its own surface, so it goes in this "+
			"table where a reviewer sees it.",
			len(appeared), strings.Join(appeared, "\n\t"))
	}
	if len(changed) > 0 {
		t.Errorf("%d extension(s) changed how much they do not declare:\n\t%s\n"+
			"\tUpdate the count in the same commit. A note added or removed without touching "+
			"this line is a change to the backlog nobody reviewed.",
			len(changed), strings.Join(changed, "\n\t"))
	}
	if len(banked) > 0 {
		t.Errorf("%d extension(s) are complete now — DELETE their line from incompleteNotes:\n\t%s\n"+
			"\tThis is the paydown the ratchet exists to bank. A stale entry claims work that "+
			"is already done, which is how a list of remaining work stops being read.",
			len(banked), strings.Join(banked, "\n\t"))
	}

	total := 0
	for _, n := range live {
		total += n
	}
	t.Logf("extensions declaring themselves partial: %d, notes: %d", len(live), total)
}
