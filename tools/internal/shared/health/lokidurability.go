package health

// lokidurability.go answers one question about a running Loki ingester: can it
// survive its own write-ahead log?
//
// THE FAILURE IT ENCODES. An ingester whose memory limit is below what WAL replay
// needs is OOMKilled mid-replay. If its WAL lives on an `emptyDir` — which
// survives CONTAINER restarts within the pod — the next attempt replays the
// identical WAL and dies identically. The loop is closed: it cannot self-heal,
// nothing about it looks like a config error, and the only escape is deleting the
// pod, which discards un-flushed chunks. Measured live on a production cluster:
// 104,337 BackOff events over 16 days with log ingestion down throughout.
//
// WHY THIS IS A PREDICATE OVER THE RUNNING POD AND NOT A CHECK ON THE VALUES.
// The override that was supposed to prevent it existed, was correct in spirit,
// and named a chart key the running topology does not read — so it applied to
// nothing while reading as a fix in every review. The lesson is that the source
// side cannot vouch for itself. These predicates deliberately know nothing about
// which values key delivered the limit: they read what the ingester actually got.
// If apl-core changes topology again, this still asks the right question.

import (
	"strconv"
	"strings"
)

// LokiIngesterSpec is the delivered state of one ingester, as read off the pod.
// Fields are strings because that is how Kubernetes reports them and because the
// failure messages must be able to print exactly what was found.
type LokiIngesterSpec struct {
	Namespace, Name string
	// MemoryLimit is the ingester container's resources.limits.memory, verbatim
	// ("1Gi"). Empty means NO limit was set, which is a distinct state from an
	// unreadable one — see LokiIngesterDurability.
	MemoryLimit string
	// WALVolumeSource names the volume type backing the ingester's data volume
	// ("emptyDir", "persistentVolumeClaim", …). Empty means the volume was not
	// found on the pod at all.
	WALVolumeSource string
	// WALStorageClass is the storageClassName of the PVC behind the WAL, when
	// there is one. Empty means either no PVC or a PVC with no class set — and
	// those are DIFFERENT, which is why WALClassKnown exists.
	//
	// IT IS CHECKED, and the reason is a bug this file's first version could not
	// see: the overlay asserted the class under a key the chart does not read
	// (`persistence.storageClass` rather than `persistence.claims[].storageClass`).
	// The PVC was still created — so a check on volume TYPE alone passed — at the
	// chart's default size with NO storageClassName, landing the WAL on the
	// default provisioner. That WAL carries the OpenBao audit stream. "It is a
	// PVC" was never the property worth asserting; "it is the encrypted class" is.
	WALStorageClass string
	// WALClassKnown distinguishes "the PVC names no class" from "we could not read
	// the PVC". Only the first is a finding; the second is a refusal to vouch.
	WALClassKnown bool
}

// LokiWALVolumeName is the chart's name for the ingester volume mounted at
// /var/loki. Named here rather than inlined because the probe that fills
// LokiIngesterSpec and the tests that exercise these predicates must agree on it,
// and a rename that split them would make this whole check quietly vacuous.
const LokiWALVolumeName = "data"

// LokiIngesterDurability grades one ingester against the WAL-replay floor,
// returning one line per property checked and whether any of them failed.
//
// TWO INDEPENDENT LINES, NOT ONE VERDICT, because the two properties fail for
// different reasons and are fixed by different keys — and because a 3Gi limit on
// an emptyDir WAL is better than 1Gi and still un-survivable. An early return on
// the limit would hide the volume finding from the operator who most needs it:
// the one whose limit fix did not end the crashloop.
//
// FAIL-CLOSED ON "COULD NOT TELL". An unparseable limit is a failure, not a pass:
// this exists to catch a silently-unapplied setting, and "we could not read the
// setting" is the same evidentiary state as "the setting is wrong". An ABSENT
// limit is reported as OK-with-a-caveat rather than a failure — an ingester with
// no memory limit cannot be OOMKilled by one, which is the specific death this
// guards against, though it can pressure its node instead.
func LokiIngesterDurability(s LokiIngesterSpec, floor, wantClass string) (msgs []string, failed bool) {
	who := s.Namespace + "/" + s.Name
	line := func(m string, bad bool) {
		msgs = append(msgs, m)
		failed = failed || bad
	}
	// The VOLUME half reports without gating; the LIMIT half gates. See
	// WALFindingsGate for the whole argument — it is not a softening, it is the
	// difference between a property this repo can deliver and one it cannot.
	vline := func(m string, bad bool) {
		if bad && !WALFindingsGate {
			msgs = append(msgs, m+"\n    ^ REPORTED, NOT GATING: "+walNotGatingReason)
			return
		}
		line(m, bad)
	}

	floorBytes, floorOK := QuantityBytes(floor)
	switch {
	case !floorOK:
		// The floor is a compile-time constant in this repo; if it stops parsing,
		// the verdict below is meaningless and must not read as a pass.
		line("FAIL: cannot parse the WAL-replay floor "+strconv.Quote(floor)+
			" — this gate cannot judge "+who+" and is not vouching for it", true)
	case strings.TrimSpace(s.MemoryLimit) == "":
		line("OK: "+who+" has no memory limit — it cannot be OOMKilled mid-WAL-replay "+
			"(it can pressure its node instead; that is a different alert)", false)
	default:
		got, ok := QuantityBytes(s.MemoryLimit)
		switch {
		case !ok:
			line("FAIL: "+who+" memory limit is "+strconv.Quote(s.MemoryLimit)+
				", which this gate cannot parse — it is NOT vouching for the WAL-replay headroom", true)
		case got < floorBytes:
			line("FAIL: "+who+" memory limit is "+s.MemoryLimit+", below the "+floor+
				" WAL-replay floor — this ingester OOMKills mid-replay and the loop cannot self-heal. "+
				"The apl-overlay asserts it at apps.loki._rawValues.ingester.resources.limits.memory "+
				"(apl-values/_shared/apl-overlay/appvalues.yaml); a limit still at the chart default "+
				"means that key names nothing in the RUNNING chart — check the deployment mode with "+
				"`kubectl -n "+s.Namespace+" get sts,deploy -l app.kubernetes.io/name=loki`", true)
		default:
			line("OK: "+who+" memory limit "+s.MemoryLimit+" meets the "+floor+" WAL-replay floor", false)
		}
	}

	switch s.WALVolumeSource {
	case "persistentVolumeClaim":
		switch {
		case !s.WALClassKnown:
			vline("FAIL: "+who+" keeps its WAL on a PVC whose StorageClass could not be read — "+
				"this gate is not vouching for it being the encrypted class", true)
		case s.WALStorageClass == "":
			vline("FAIL: "+who+" keeps its WAL on a PVC with NO storageClassName, so it landed on "+
				"the cluster default provisioner rather than "+wantClass+". The WAL carries "+
				"un-flushed log lines including the OpenBao audit stream, so this is an "+
				"unencrypted copy of them. The overlay asserts the class at "+
				"apps.loki._rawValues.ingester.persistence.claims[].storageClass — note CLAIMS: "+
				"a storageClass one level up is accepted by Helm and read by nothing", true)
		case s.WALStorageClass != wantClass:
			vline("FAIL: "+who+" keeps its WAL on StorageClass "+strconv.Quote(s.WALStorageClass)+
				", not "+strconv.Quote(wantClass)+" — the encrypted, retain-policy class. Loki is "+
				"not covered by cluster.defaultStorageClass on managed, so an unpinned class is "+
				"whatever the cluster defaults to", true)
		default:
			vline("OK: "+who+" keeps its WAL on a "+wantClass+" PVC", false)
		}
	case "":
		vline("FAIL: "+who+" has no "+strconv.Quote(LokiWALVolumeName)+" volume — this gate "+
			"cannot see where the WAL lives and is not vouching for it (chart volume renamed?)", true)
	case "emptyDir":
		vline("FAIL: "+who+" keeps its WAL on an emptyDir. An emptyDir survives CONTAINER "+
			"restarts, so an OOM during replay replays the identical WAL and dies identically — "+
			"the crashloop cannot self-heal and the only escape discards un-flushed chunks. "+
			"The apl-overlay asserts a PVC at apps.loki._rawValues.ingester.persistence "+
			"(apl-values/_shared/apl-overlay/appvalues.yaml)", true)
	default:
		// FAIL, not OK, and the earlier version had this exactly backwards: it
		// graded an unrecognised source a PASS while its own message described the
		// non-durability the emptyDir arm fails on. A generic ephemeral volume, a
		// hostPath, an inMemory ramdisk (`persistence.inMemory: true` is a real
		// chart option) all land here, and none of them survives pod recreation —
		// which is the property this whole check exists to assert.
		vline("FAIL: "+who+" keeps its WAL on a "+strconv.Quote(s.WALVolumeSource)+" volume, which "+
			"is not a PVC — un-flushed chunks do not survive pod recreation, and this gate does not "+
			"recognise the type well enough to say more. If this source IS durable, add it to the "+
			"PVC arm; passing it silently is how an unrecognised volume becomes a vouched-for one", true)
	}
	return msgs, failed
}

// QuantityBytes converts a Kubernetes quantity ("8Gi", "500Mi", "1.5Ti",
// "1000000") to bytes. Binary (Ki/Mi/Gi/Ti/Pi) and decimal (k/M/G/T/P) suffixes
// are both accepted.
//
// ok IS THE POINT, and it is why this is not the older unexported copy in
// brownfield: that one folds an unparseable value into 0, which a >= comparison
// then reads as "smaller than everything" and a <= comparison reads as "fits".
// A gate needs to tell "zero" from "could not tell", so the answer is two values.
func QuantityBytes(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	units := []struct {
		suf string
		mul float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suf) {
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suf)), 64)
			if err != nil {
				return 0, false
			}
			return int64(f * u.mul), true
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return int64(f), true
}

// WALFindingsGate decides whether a WAL-VOLUME finding fails the lane. The
// memory-limit finding always does.
//
// FALSE, AND THE REASON IS NOT TIMIDITY. LLZ asserts the WAL PVC through the
// apl-overlay, which reaches apl-core AFTER apl-operator has already created the
// loki-ingester StatefulSet. `volumeClaimTemplates` is immutable on an existing
// StatefulSet, so the apiserver rejects the update — on a fresh cluster exactly as
// on an old one. There is no ordering in which this channel can deliver a PVC:
// every cluster needs a one-time manual StatefulSet recreation
// (docs/upstream-asks.md §1), and until apl-core ships the PVC itself, gating
// would mean a permanently red lane on every cluster including every fresh e2e
// run — a gate nobody can turn green gets turned off, and takes the memory check
// with it.
//
// So the finding is REPORTED, loudly, on every run, and the operator decides when
// to migrate. This mirrors the #397 deferral in lokiwrites.go: the same shape, the
// same reason, and the same rule — when this becomes deliverable, flip the
// constant rather than deleting the check.
//
// THE LIMIT HALF STAYS GATING because it IS deliverable: `resources` lives in the
// pod template, which a StatefulSet update may change, so an ingester below the
// WAL-replay floor is a condition this repo can actually fix.
const WALFindingsGate = false

// walNotGatingReason is printed beside every non-gating WAL finding so a reader
// meeting one never has to find this file to learn why it did not fail the lane.
const walNotGatingReason = "the WAL PVC cannot be delivered by the apl-overlay — " +
	"volumeClaimTemplates is immutable and apl-operator creates the StatefulSet first, so every " +
	"cluster needs the one-time recreation in docs/upstream-asks.md §1. The finding is real; it is " +
	"not failing the lane because no cluster can currently satisfy it. The memory-limit check above " +
	"DOES gate."
