package volumes

// legacy_labels_test.go preserves the label scheme the RETIRED volume-labels lane
// used to write: `<REGION_SHORT>-<namespace>-<pvc>`, sanitized to Linode's charset
// and squeezed into the 32-char cap.
//
// Nothing writes these labels any more — renaming a bound Volume is what breaks
// its next mount (see the retirement commit, and the drift leg of
// assert-volume-encryption). But clusters relabelled by older builds still CARRY
// them, and `llz reap` still has to see them or those Volumes leak on teardown.
// So the generator survives as a TEST FIXTURE: it is how the reaper-contract test
// below constructs a realistic legacy label to check
// linode.VolumeLabelPrefixes against. It is deliberately not production code —
// production only needs to RECOGNISE the prefix, never to produce the name.

import (
	"encoding/json"
	"strings"
)

const (
	maxLinodeLabel = 32
)

// jnum builds the json.Number the Linode client yields for numeric fields. It
// moved here when the volume-labels lane was retired: it describes the API shape
// the surviving tests still need, not anything the lane owned. (Its companion
// `pv` helper went with the lane — every caller was a relabeler test.)
func jnum(n string) json.Number { return json.Number(n) }

// desiredVolumeLabel builds the target Linode label: <region>-<namespace>-<pvc>,
// mapping every char outside Linode's [A-Za-z0-9_-] set to '-', truncating to the
// 32-char cap, then stripping any trailing '-' left by truncation. Mirrors
// relabel.sh's `tr -c 'A-Za-z0-9_-' '-' | cut -c -32 | sed 's/-*$//'`.
func desiredVolumeLabel(regionShort, namespace, pvcName string) string {
	raw := regionShort + "-" + namespace + "-" + pvcName
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return fitLinodeLabel(b.String())
}

// labelTailKeep is how much of the RIGHT-hand side survives truncation. Sized to
// carry a StatefulSet ordinal plus enough of the claim name to tell siblings apart
// (`…enbao-0`, `…-db-1-wal`).
const labelTailKeep = 8

// fitLinodeLabel squeezes a label into Linode's 32-char cap while KEEPING the
// discriminating tail, by dropping from the middle rather than the end.
//
// The naive `s[:32]` this replaces cut off exactly the part that distinguishes
// sibling volumes, so an entire StatefulSet collapsed to one label:
//
//	e2e-llz-openbao-data-platform-openbao-0 ┐
//	e2e-llz-openbao-data-platform-openbao-1 ├─► "e2e-llz-openbao-data-platform-op"
//	e2e-llz-openbao-data-platform-openbao-2 ┘
//
// Linode Volume labels are account-UNIQUE, so the first replica won and the other
// two failed `PUT /v4/volumes/<id>` with 400 {"reason":"Must be unique"} — for the
// entire life of the relabeler — observed live, 17 of 17 renames rejected, which
// is why every Volume kept its opaque pvc-<uuid> label.
//
// Truncation is still lossy, so this is not a uniqueness GUARANTEE — two claims
// agreeing on both the head and the last 8 characters would still collide. It
// removes the systematic collision (StatefulSet replicas, which differ only in the
// final character) rather than every conceivable one. The remaining risk surfaces
// as a loud per-volume error plus a red `assert-volume-encryption`, not silence.
func fitLinodeLabel(s string) string {
	if len(s) <= maxLinodeLabel {
		return strings.TrimRight(s, "-")
	}
	head := maxLinodeLabel - labelTailKeep - 1 // -1 for the joining '-'
	tail := strings.TrimLeft(s[len(s)-labelTailKeep:], "-")
	out := strings.TrimRight(s[:head], "-") + "-" + tail
	return strings.TrimRight(out, "-")
}
