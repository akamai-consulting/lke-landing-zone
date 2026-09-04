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
	// WALReplayCeiling is `ingester.wal.replay_memory_ceiling` as it appears in
	// the config the ingester actually LOADED ("1536MB"). Empty means the key is
	// absent, and absent is not a neutral default here: it means Loki's own 4GB
	// default is in force, which above a 3Gi container limit is unreachable by
	// construction. That distinction is the whole point of reading it.
	WALReplayCeiling string
	// WALCeilingKnown distinguishes "the loaded config sets no ceiling" from "we
	// could not read the config at all". Only the first is a finding about the
	// cluster; the second is this gate declining to vouch, and collapsing them
	// would turn an unreadable ConfigMap into a confident diagnosis of the one
	// bug this predicate exists to name.
	WALCeilingKnown bool
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

	// The CEILING half. Read as a RELATION against the delivered limit rather
	// than against a constant: what kills an ingester is not a particular number,
	// it is a ceiling the cgroup can never let it reach. That framing keeps this
	// correct if either side is retuned, and it is why no expected value is
	// passed in.
	switch {
	case !s.WALCeilingKnown:
		line("FAIL: "+who+" — could not read the Loki config that would carry "+
			"ingester.wal.replay_memory_ceiling, so this gate is NOT vouching that WAL replay "+
			"fits inside the memory limit. 'Could not tell' is the same evidentiary state as "+
			"'wrong' for a setting whose absence OOMKills", true)
	case strings.TrimSpace(s.WALReplayCeiling) == "":
		line("FAIL: "+who+" sets no ingester.wal.replay_memory_ceiling, so Loki's own 4GB "+
			"default is in force. Above a smaller container limit that ceiling is unreachable: "+
			"replay grows toward it, the flush that would drain the WAL never fires, and the "+
			"kernel OOMKills mid-replay — every retry replaying the same WAL. The apl-overlay "+
			"asserts it at apps.loki._rawValues.loki.ingester.wal.replay_memory_ceiling "+
			"(apl-values/_shared/apl-overlay/appvalues.yaml); note the loki.ingester path — the "+
			"TOP-LEVEL ingester key renders the StatefulSet and never reaches Loki's config", true)
	default:
		ceil, ceilOK := LokiByteSize(s.WALReplayCeiling)
		limit, limitOK := QuantityBytes(s.MemoryLimit)
		switch {
		case !ceilOK:
			line("FAIL: "+who+" replay_memory_ceiling is "+strconv.Quote(s.WALReplayCeiling)+
				", which this gate cannot parse — it is NOT vouching that replay fits", true)
		case strings.TrimSpace(s.MemoryLimit) == "":
			line("OK: "+who+" replay ceiling "+s.WALReplayCeiling+" with no memory limit to "+
				"exceed — nothing can OOMKill it on the ceiling", false)
		case !limitOK:
			// Already FAILed above on the limit itself; say why the ceiling verdict
			// is missing rather than emitting a pass that rests on the same
			// unreadable number.
			line("FAIL: "+who+" replay ceiling "+s.WALReplayCeiling+" cannot be compared against "+
				"an unparseable memory limit "+strconv.Quote(s.MemoryLimit)+" — not vouching", true)
		case ceil >= limit:
			line("FAIL: "+who+" replay_memory_ceiling "+s.WALReplayCeiling+" is not below its "+
				"memory limit "+s.MemoryLimit+", so the ceiling can never be reached: the process "+
				"is OOMKilled before Loki decides to flush. This is the closed crashloop, not a "+
				"tuning nit — a WAL larger than the limit can never drain", true)
		default:
			line("OK: "+who+" replay ceiling "+s.WALReplayCeiling+" fits inside its "+
				s.MemoryLimit+" limit — replay flushes and continues instead of OOMKilling", false)
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

// LokiByteSize converts a Loki `flagext.ByteSize` spelling ("4GB", "1536MB",
// "1.5GiB", "1073741824") to bytes.
//
// A SECOND PARSER, DELIBERATELY, and not a bug that it sits beside
// QuantityBytes. The two values it and QuantityBytes read are written in
// different grammars by different systems: the memory limit is a Kubernetes
// quantity (Mi/Gi binary, M/G decimal, no trailing B), while the replay ceiling
// is parsed by Loki through go-humanize (MB/GB decimal, MiB/GiB binary, trailing
// B expected). Feeding "1536MB" to QuantityBytes fails — it looks for an "M"
// suffix and finds "B" — and a gate that cannot parse the value it is judging
// fails closed, so one parser for both would report the ceiling unreadable on
// every correctly-configured cluster.
//
// ok RATHER THAN A ZERO, for the reason QuantityBytes gives: a gate must be able
// to tell "zero" from "could not tell", and a ceiling folded to 0 would compare
// as comfortably below every limit — a pass manufactured out of a parse failure.
func LokiByteSize(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// Longest and most specific first: "MiB" must win over "MB" and "B", or
	// "1536MiB" is read as 1536 bytes with an "MiB"-shaped tail.
	units := []struct {
		suf string
		mul float64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40}, {"PiB", 1 << 50},
		{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"PB", 1e15},
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
		{"B", 1},
	}
	for _, u := range units {
		if len(s) <= len(u.suf) || !strings.EqualFold(s[len(s)-len(u.suf):], u.suf) {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s[:len(s)-len(u.suf)]), 64)
		if err != nil || f < 0 {
			return 0, false
		}
		return int64(f * u.mul), true
	}
	// No suffix at all is a plain byte count, which flagext.ByteSize accepts.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return int64(f), true
}
