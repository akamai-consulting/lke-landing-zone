package volumes

// ci_assert_volume_encryption.go is `llz ci assert-volume-encryption` — the e2e
// gate for the storage invariant. Every Linode Volume backing a PV in this cluster
// must be:
//
//   1. ENCRYPTED at rest (the security property, and the reason for the name);
//   2. tagged with this cluster's `lke<id>`, so `llz reap` can attribute it;
//   3. labelled `<region>-<ns>-<pvc>` rather than the CSI's default `pvc-<uuid>`,
//      so it is identifiable in the Linode UI, the billing export and the quota
//      census — the places you look when the cluster is already gone.
//
// (1) is fixed at CreateVolume or never. (2) and (3) are applied afterwards by
// llz-reconciler lanes, so this gate waits a bounded time for them before failing —
// see volumeHealBudget.
//
// It asserts against the LINODE API, not against Kubernetes. That is the whole
// point. The obvious cheaper check — "is every PVC on block-storage-retain?" — is a
// PROXY: it infers encryption from the name of the class the PVC asked for. That
// proxy is exactly what was in place while 13 of 16 PVCs on a live cluster
// provisioned unencrypted, and it can be satisfied by a class whose parameters say nothing
// about encryption, or defeated by a class that encrypts under a different name.
// `encryption: "enabled"` on the Volume object is the ground truth, and it is the
// only thing that survives someone renaming or re-parameterising a StorageClass.
//
// FAIL-CLOSED on every ambiguity: an unreachable cluster, an unreachable Linode
// API, a Volume the API will not return, and — deliberately — a cluster with NO
// Linode-CSI PVs at all. A security gate that reports "nothing wrong" when it
// examined nothing is worse than no gate, because it launders an absence of
// evidence into a green check.
//
// Encryption is decided inside CreateVolume and `storageClassName` is immutable
// once bound, so a red run here is NOT re-runnable: it means the workload must be
// re-rolled onto a class that encrypts. The output says so.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/linode"
)

// volumeEncryptionEnabled is the Linode API's value for an encrypted Volume.
// The field is absent on volumes predating the feature, which reads as NOT
// encrypted — the correct bias.
const volumeEncryptionEnabled = "enabled"

// volumeHealBudget / volumeHealInterval bound the wait for the volume-tags and
// volume-labels reconciler lanes to sweep. Both apply AFTER CreateVolume, so
// asserting the instant converge finishes would be asserting on a race. Encryption
// is never waited on — it cannot change.
const (
	volumeHealBudget   = 3 * time.Minute
	volumeHealInterval = 20 * time.Second
)

// Clock seams so the retry loop is unit-testable without real waiting.
var (
	assertVolumeNow   = time.Now
	assertVolumeSleep = time.Sleep
)

// volumeVerdict is one PV-backed Volume's compliance with the storage invariant.
type volumeVerdict struct {
	pvVolume
	Label       string
	Encryption  string
	MissingTags []string
	// BadLabel is non-empty when the Volume's Linode label is not the readable
	// <region>-<ns>-<pvc> the volume-labels reconciler is supposed to give it.
	BadLabel string
	// NotReapable is non-empty when reap's OWN predicate would not select this
	// Volume once it detaches — i.e. the destroy-time sweep is blind to it.
	NotReapable string
	// Unreachable records why the Volume could not be judged. A Volume we cannot
	// read is a FAILURE, never a pass — see the fail-closed note in the header.
	Unreachable string
}

func (v volumeVerdict) ok() bool {
	return v.Unreachable == "" && v.Encryption == volumeEncryptionEnabled &&
		len(v.MissingTags) == 0 && v.BadLabel == "" && v.NotReapable == ""
}

// healable reports whether a reconciler lane can still fix this Volume. Tags
// (volume-tags lane) and labels (volume-labels lane) are healed asynchronously
// after CreateVolume, so a violation may simply mean the lane has not run yet.
// Encryption is NOT healable — it is decided inside CreateVolume and
// storageClassName is immutable once bound — so an encryption violation is final
// the moment it is observed, and waiting on it only wastes the budget.
func (v volumeVerdict) healable() bool {
	// NotReapable is a naming/predicate mismatch in code, not a pending reconcile —
	// waiting cannot change it, so it is final like encryption.
	//
	// BadLabel IS FINAL TOO, and it did not used to be. When this gate demanded
	// that Volumes had been RENAMED off the CSI default, a bad label meant "the
	// volume-labels lane has not got to it yet" and waiting was the right answer.
	// That leg is inverted: BadLabel now means the live label has DRIFTED from the
	// one in the PV's immutable volumeHandle, and nothing renames Volumes any more,
	// so no amount of polling changes it. Treating it as healable burned the whole
	// heal budget re-listing the Linode API and then blamed a lane that no longer
	// exists.
	return v.Unreachable == "" && v.Encryption == volumeEncryptionEnabled &&
		v.NotReapable == "" && v.BadLabel == ""
}

// problem renders the single most important thing wrong with this Volume.
func (v volumeVerdict) problem() string {
	switch {
	case v.Unreachable != "":
		return "UNREADABLE: " + v.Unreachable
	case v.Encryption != volumeEncryptionEnabled:
		enc := v.Encryption
		if enc == "" {
			enc = "<unset>"
		}
		return "NOT ENCRYPTED (encryption=" + enc + ")"
	case v.NotReapable != "":
		return v.NotReapable
	case len(v.MissingTags) > 0:
		return "untagged: missing " + strings.Join(v.MissingTags, ",")
	default:
		return v.BadLabel
	}
}

// judgeVolume compares one fetched Volume against the invariant. Pure, so the
// classification is testable without a Linode account.
//
// regionShort empty means "REGION_SHORT was not available", in which case the label
// is only checked for NOT being the CSI default — a weaker but still meaningful
// assertion, and better than skipping the check because one env var is missing.
func judgeVolume(pv pvVolume, vol map[string]any, desired []string, regionShort string) volumeVerdict {
	v := volumeVerdict{pvVolume: pv}
	if vol == nil {
		v.Unreachable = "Linode API returned no volume"
		return v
	}
	v.Label = linode.MapString(vol, "label")
	v.Encryption = linode.MapString(vol, "encryption")

	have := make(map[string]bool)
	for _, t := range linode.MapTags(vol) {
		have[t] = true
	}
	for _, want := range desired {
		if !have[want] {
			v.MissingTags = append(v.MissingTags, want)
		}
	}
	sort.Strings(v.MissingTags)

	// DRY-RUN REAP. Run reap's OWN selection predicate against this Volume with
	// detachment simulated (linodeIDNull=true), asking: once the cluster is
	// destroyed and this Volume detaches, would the destroy-time sweep actually
	// pick it up?
	//
	// This exists because the answer was silently NO for months. reap selects on a
	// label prefix; the volume-labels reconciler renames Volumes; nobody re-checked
	// that the new names still matched. Destroying one cluster leaked all 15 of its
	// renamed Volumes, which then squatted their account-unique labels and broke
	// relabeling on the next cluster. Neither subsystem was individually wrong — their COUPLING
	// was, and nothing tested it.
	//
	// Deliberately calls linode.VolumeIsCandidate / VolumeLabelPrefixes rather than
	// re-implementing the rule: the point is to pin naming against the real reaper,
	// so changing either side without the other turns this red. A literal `reap
	// --dry-run` cannot do this job — reap only considers UNATTACHED Volumes, and a
	// live cluster's are attached, so it would match nothing and prove nothing.
	// Only meaningful when REGION_SHORT is known: the accepted prefix set is
	// {"pvc-", "<env>-"}, so without env every RELABELED Volume would look
	// unreapable and this check would fire on a healthy cluster. Skipping is right
	// here — unlike the label and encryption checks, a wrong answer would be a
	// false alarm about a destructive sweep, and the destroy path passes --env.
	if regionShort != "" && !linode.VolumeIsCandidate(true, v.Label, linode.MapString(vol, "region"),
		linode.MapTags(vol), "", nil, pv.VolumeID, "", linode.VolumeLabelPrefixes(regionShort)...) {
		v.NotReapable = fmt.Sprintf("label %q is INVISIBLE to reap (accepted prefixes: %v) — this Volume would survive its own cluster's destroy and leak", v.Label, linode.VolumeLabelPrefixes(regionShort))
	}

	// Label is only meaningful for a BOUND PV.
	//
	// A claimRef test is not enough, and that gap made this gate permanently red on
	// any cluster with Retain-policy leftovers. A RELEASED PV keeps its claimRef —
	// that is what makes it Released rather than Available — so every leaked PV from
	// every previous incarnation of a StatefulSet pod still looks claimed. With
	// Retain, `monitoring/storage-loki-0` accumulates one per cluster rebuild.
	//
	// desiredVolumeLabel is a pure function of (region, namespace, pvc), so all of
	// them want the SAME label, and Linode Volume labels are ACCOUNT-UNIQUE: the
	// first rename wins and the rest fail duplicate-label forever. Demanding a
	// readable label on those is asserting something that cannot happen — the lane
	// is not slow, it is impossible. Observed as "5 of 22 … still the CSI default",
	// all five claiming monitoring/storage-loki-0.
	//
	// They stay reapable: reap accepts the `pvc-` prefix, which is what they keep.
	// THIS CHECK IS INVERTED FROM WHAT IT USED TO BE, and the inversion is the
	// point. It used to demand that a bound volume had been RENAMED off the CSI
	// default to <region>-<ns>-<pvc>, and would flag a `pvc-<uuid>` label as "the
	// volume-labels reconciler has not renamed it". That renaming is what breaks
	// the mount: the CSI resolves the device path from the label baked into the
	// PV's immutable volumeHandle, so a volume renamed away from it can never be
	// mounted again — immediately if the pod had not mounted yet, or latently on
	// the next drain/reschedule. The old gate therefore enforced the defect and
	// would have gone red on the fix. See relabel_volumes.go's header.
	//
	// So the question is no longer "was it renamed?" but "does its live label still
	// match the one the CSI will ask for?". That catches the legacy clusters too:
	// volumes relabelled by builds predating the fix are still unmountable, and an
	// operator wants to know which ones before a drain finds out for them.
	if pv.Namespace != "" && pv.PVC != "" && (pv.Phase == "" || pv.Phase == "Bound") {
		switch {
		case v.Label == "":
			v.BadLabel = "Linode Volume has NO label"
		case pv.HandleLabel != "" && v.Label != pv.HandleLabel:
			v.BadLabel = fmt.Sprintf("label %q was RENAMED away from %q, the label in the PV's volumeHandle — the Linode CSI looks the device up by that immutable name, so this volume will fail to mount on its next attach (drain, reschedule, upgrade)", v.Label, pv.HandleLabel)
		}
	}
	return v
}

func AssertEncryption(ctx context.Context, d Deps, scName string) error {
	token := d.Token
	if token == "" {
		return fmt.Errorf("assert-volume-encryption: LINODE_TOKEN must be set (env or the optional linode-api-token Secret volume) — without it this check cannot read Volume encryption state, and skipping it would be a silent pass")
	}
	if scName == "" {
		scName = DefaultTagsSC
	}

	// REGION_SHORT is the prefix `llz reap` derives, and the one the RETIRED
	// relabeler used to write. It no longer feeds the label check at all — that
	// compares a Volume's live label against its PV's volumeHandle, which needs no
	// per-env input — and is read only by the reapability leg below. Optional, so a
	// missing env var narrows that leg rather than silently disabling the gate.
	regionShort := os.Getenv("REGION_SHORT")
	if regionShort == "REPLACE_ME" {
		// The un-rendered placeholder from the reconciler manifest. Treat as unset:
		// comparing against "REPLACE_ME-<ns>-<pvc>" would fail every Volume for the
		// wrong reason.
		regionShort = ""
	}

	// Read the cluster through kubectl, NOT the in-pod client the sibling
	// reconcile-volume-tags lane uses: this runs on a CI runner against a fetched
	// kubeconfig, like every other `llz ci assert-*` in the e2e suite. discoverKubeFn
	// is in-cluster only and would fail here.
	scRaw, err := d.Kubectl("get", "storageclass", scName, "-o", "json")
	if err != nil {
		return fmt.Errorf("assert-volume-encryption: get storageclass %s: %w", scName, err)
	}
	var sc map[string]any
	if err := json.Unmarshal(scRaw, &sc); err != nil {
		return fmt.Errorf("assert-volume-encryption: parse storageclass %s: %w", scName, err)
	}
	desired, err := desiredTagsFromSC(sc, scName)
	if err != nil {
		return err
	}

	pvRaw, err := d.Kubectl("get", "persistentvolumes", "-o", "json")
	if err != nil {
		return fmt.Errorf("assert-volume-encryption: list persistentvolumes: %w", err)
	}
	var pvList map[string]any
	if err := json.Unmarshal(pvRaw, &pvList); err != nil {
		return fmt.Errorf("assert-volume-encryption: parse persistentvolumes: %w", err)
	}
	pvs := parsePVVolumes(pvList)
	if len(pvs) == 0 {
		return fmt.Errorf("assert-volume-encryption: found NO Linode-CSI PersistentVolumes. On a converged cluster the platform always has some (openbao, harbor, keycloak, monitoring), so this means the cluster is not converged or PV discovery broke — either way there is nothing to attest and a pass here would be vacuous")
	}

	client := tagReconcileLinodeFn(token)

	// Tags and labels are applied by reconciler lanes AFTER CreateVolume, so a
	// violation of either can simply mean the lane has not swept yet. Give those a
	// bounded chance to heal rather than asserting on a race. Encryption is not
	// healable, so the moment any Volume is unencrypted the answer is final and this
	// returns immediately — no reason to burn the budget on a verdict that cannot
	// change.
	var verdicts []volumeVerdict
	deadline := assertVolumeNow().Add(volumeHealBudget)
	for attempt := 1; ; attempt++ {
		verdicts = verdicts[:0]
		for _, pv := range pvs {
			vol, status, err := client.Volume(ctx, pv.VolumeID)
			switch {
			case err != nil:
				verdicts = append(verdicts, volumeVerdict{pvVolume: pv, Unreachable: fmt.Sprintf("GET: %v", err)})
				continue
			case status < 200 || status >= 300:
				verdicts = append(verdicts, volumeVerdict{pvVolume: pv, Unreachable: fmt.Sprintf("GET returned %d", status)})
				continue
			}
			verdicts = append(verdicts, judgeVolume(pv, vol, desired, regionShort))
		}

		pending := 0
		for _, v := range verdicts {
			if v.ok() {
				continue
			}
			if !v.healable() {
				pending = 0 // a final violation exists; stop waiting
				break
			}
			pending++
		}
		if pending == 0 || !assertVolumeNow().Before(deadline) {
			if pending > 0 {
				fmt.Printf("::warning::%d Volume(s) still missing tags after %s — the volume-tags reconciler lane did not heal them in time.\n",
					pending, volumeHealBudget)
			}
			break
		}
		fmt.Printf("attempt %d: %d Volume(s) awaiting tag/label reconciliation — re-checking in %s (budget %s)\n",
			attempt, pending, volumeHealInterval, volumeHealBudget)
		assertVolumeSleep(volumeHealInterval)
	}

	return reportVolumeEncryption(d, verdicts, desired, scName)
}

// reportVolumeEncryption prints the verdict and returns the pass/fail. Split out so
// the reporting is exercised by tests without a cluster or a Linode account.
func reportVolumeEncryption(d Deps, verdicts []volumeVerdict, desired []string, scName string) error {
	var bad []volumeVerdict
	for _, v := range verdicts {
		if !v.ok() {
			bad = append(bad, v)
		}
	}
	if len(bad) == 0 {
		fmt.Printf("All %d PV-backed Linode Volume(s) are encrypted at rest and carry %s.\n",
			len(verdicts), strings.Join(desired, ","))
		return nil
	}

	fmt.Printf("::error::%d of %d PV-backed Linode Volume(s) violate the storage invariant (required tags from StorageClass %s: %s)\n",
		len(bad), len(verdicts), scName, strings.Join(desired, ","))
	lines := make([]string, 0, len(bad))
	for _, v := range bad {
		where := v.Namespace + "/" + v.PVC
		if v.Namespace == "" && v.PVC == "" {
			where = "<released PV, no claimRef>"
		}
		line := fmt.Sprintf("volume %s (%s) %s — %s", v.VolumeID, v.Label, where, v.problem())
		lines = append(lines, line)
		fmt.Printf("::error::  %s\n", line)
	}

	summary := append([]string{
		"### Linode Volumes violating the storage invariant",
		"",
		fmt.Sprintf("%d of %d PV-backed Volumes are unencrypted, untagged, or unreadable.", len(bad), len(verdicts)),
		"",
		"```",
	}, lines...)
	summary = append(summary,
		"```",
		"",
		"Three different invariants are checked, and they do NOT have the same remedy:",
		"",
		"**NOT ENCRYPTED — not repairable in place.** Encryption is decided inside",
		"CreateVolume and `storageClassName` is immutable once bound, so re-running this",
		"job cannot turn it green. Remediation is per-workload and",
		"destroys that volume's data: delete the owning workload, delete the PVC, re-sync",
		"so it is recreated on a class that encrypts.",
		"",
		"**UNTAGGED — repairable, and something should already have done it.** Tags are",
		"applied after CreateVolume by the `--reconcile-volume-tags` llz-reconciler lane,",
		"and this gate already waited for it. Seeing this here means the lane is not",
		"running, not electing a leader, or has no LINODE_TOKEN — it reads the token",
		"lazily from the optional `linode-api-token` Secret and silently no-ops when it is",
		"absent, so check that Secret exists before suspecting anything subtler.",
		"",
		"**RENAMED LABEL — final, and DO NOT try to fix it by renaming.** A `pvc-<uuid>`",
		"label is the CORRECT state: the Linode CSI resolves a Volume's device path from",
		"the label baked into its PV's immutable volumeHandle, so a Volume whose live",
		"label has drifted from that one cannot be mounted on its next attach — a drain,",
		"a reschedule, an upgrade. Renaming it again only moves the target. Recover it",
		"WITH ITS DATA by restoring the exact original label (printed above, and it is the",
		"`<label>` half of the PV's volumeHandle), or by recreating the PV object against",
		"`<id>-<current-label>` under Retain. See docs/runbooks/volume-labels.md.",
		"Workload identity lives in Volume TAGS (`ns-<namespace>`, `<namespace>-<pvc>`),",
		"which no device lookup reads.",
		"",
		"**If these landed on an LKE stock class** (`linode-block-storage[-retain]`), the",
		"cause is upstream of the PVC: on managed, apl-core's `cluster.defaultStorageClass`",
		"is Linode's unencrypted class and its PVCs name it explicitly. Check that",
		"`llz ci bootstrap-cluster` recreated the stock classes encrypted, and that LKE has",
		"not re-promoted its own unencrypted definitions since.")
	if err := d.Summary(summary...); err != nil {
		return err
	}
	return fmt.Errorf("assert-volume-encryption: %d of %d PV-backed Linode Volume(s) are not encrypted-and-tagged", len(bad), len(verdicts))
}
