package clusterspec

// velero.go — the velero component's per-env values: where its backups go, how
// often, for how long, and over which namespaces. One file because the three
// consumers of these rules — Validate (what an author may write), RenderVeleroPatch
// (what the cluster runs) and the backup drill (what it re-checks on the live
// Schedule) — must not each hold a copy of the namespace rule. That rule is the
// one this component cannot get wrong: llz-openbao never enters a backup.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// VeleroNamespace is where the server, its Schedule and every Backup and
	// Restore CR live.
	VeleroNamespace = "llz-velero"
	// VeleroScheduleName is the Schedule the chart renders for the `cluster` key
	// under release `velero` (`<fullname>-<key>`). The alerts select on it as the
	// metrics' schedule label, and the drill builds its Backup from it.
	VeleroScheduleName = "velero-cluster"
	// VeleroApplication is the Argo Application (inside the llz-velero carved App)
	// that deploys the chart, and the target of RenderVeleroPatch.
	VeleroApplication = "llz-velero-server"

	// VeleroDefaultIntervalHours is the default schedule: one backup a day. The
	// knob shares openbaoBackup's rule (a divisor of 24) so `*/N` is a regular
	// interval.
	VeleroDefaultIntervalHours = 24
	// VeleroDefaultRetention is the default TTL: thirty days of daily backups.
	VeleroDefaultRetention = "30d"
	// VeleroMaxRetentionHours bounds the TTL at a year. Past that the bucket is an
	// archive of Secrets nobody rotates out of it, and the versioned bucket adds
	// velero_noncurrent_days on top of whatever is set here.
	VeleroMaxRetentionHours = 366 * 24
	// veleroMinute is off the hour, and off openbaoBackup's :23, so the two
	// backups and the 02:00/03:00 sweeps do not stack.
	veleroMinute = 37
	// veleroDailyHour places the default daily run in the small hours UTC.
	veleroDailyHour = 1
)

// VeleroMandatoryExcludes are excluded from EVERY Velero schedule, whatever the
// spec says, and cannot be removed by backupExcludeNamespaces:
//
//   - llz-openbao: its openbao-unseal-key Secret IS OPENBAO_SEAL_KEY, and its Raft
//     PVCs are the store. Backing both up puts the seal key beside the data, so
//     read access to the Velero bucket alone would be the whole of OpenBao.
//     OpenBao is backed up by openbaoBackup and restored only from those snapshots.
//   - llz-velero: Velero's own object-storage key and the SSE-C key its backups
//     are encrypted under. A backup that carries its own decryption key protects
//     nothing.
var VeleroMandatoryExcludes = []string{"llz-openbao", VeleroNamespace}

// VeleroDefaultExcludes are excluded unless backupExcludeNamespaces replaces
// them. Each holds credential material that is re-derived on a rebuild and is
// worth nothing in a restore, so copying it into one more bucket only widens who
// can read it:
//
//   - kube-system, kube-public, kube-node-lease: LKE-E's, recreated by the
//     provider; restoring them fights it.
//   - cert-manager: the llz-client-ca and openbao-bootstrap-ca private keys — a
//     copy is a way to mint client certificates OpenBao trusts.
//   - istio-system: the mesh CA key.
//   - external-secrets: ESO's mTLS identity to OpenBao.
//   - llz-openbao-backup, llz-pat-rotator, llz-reconciler, obj-proxy: LLZ
//     workloads whose only state is credentials from OpenBao (the account-wide
//     broad PAT, the objProxy SSE-C key, …) and GitOps-rendered objects.
var VeleroDefaultExcludes = []string{
	"kube-system", "kube-public", "kube-node-lease",
	"cert-manager", "istio-system", "external-secrets",
	"llz-openbao-backup", "llz-pat-rotator", "llz-reconciler", "obj-proxy",
}

// veleroNSRe admits a namespace name, or a Velero glob over one (Velero 1.18
// expands `*` in namespace filters).
var veleroNSRe = regexp.MustCompile(`^[a-z0-9*]([-a-z0-9*]{0,61}[a-z0-9*])?$`)

// veleroDurationRe is durationRe with the number and unit captured.
var veleroDurationRe = regexp.MustCompile(`^([0-9]+)(ns|us|µs|ms|s|m|h|d|w|y)$`)

// VeleroScope is the namespace scope a schedule runs with.
type VeleroScope struct {
	Include, Exclude []string
}

// VeleroScopeFor resolves the toggle's namespace knobs into the Schedule's lists:
// include defaults to "*", exclude to VeleroDefaultExcludes, and the mandatory
// excludes are ALWAYS appended (deduplicated, order stable). Pure.
func VeleroScopeFor(t ComponentToggle) VeleroScope {
	include := strings.Fields(t.BackupIncludeNamespaces)
	if len(include) == 0 {
		include = []string{"*"}
	}
	exclude := strings.Fields(t.BackupExcludeNamespaces)
	if len(exclude) == 0 {
		exclude = append([]string(nil), VeleroDefaultExcludes...)
	}
	seen := map[string]bool{}
	var ex []string
	for _, n := range append(append([]string(nil), VeleroMandatoryExcludes...), exclude...) {
		if !seen[n] {
			seen[n] = true
			ex = append(ex, n)
		}
	}
	return VeleroScope{Include: include, Exclude: ex}
}

// VeleroScopeProtectsOpenBao reports whether a schedule scope keeps every
// mandatory namespace out: each is excluded (Velero applies excludes after
// includes, so an exclude wins over any include or glob) and none is named in the
// include list. The drill holds the LIVE Schedule to this; the render test holds
// every rendered patch to it.
func VeleroScopeProtectsOpenBao(s VeleroScope) error {
	ex := map[string]bool{}
	for _, n := range s.Exclude {
		ex[n] = true
	}
	for _, m := range VeleroMandatoryExcludes {
		if !ex[m] {
			return fmt.Errorf("the schedule does not exclude %s (excludedNamespaces %v) — it would back up %s", m, s.Exclude, veleroWhy(m))
		}
	}
	for _, n := range s.Include {
		for _, m := range VeleroMandatoryExcludes {
			if n == m {
				return fmt.Errorf("the schedule INCLUDES %s by name (includedNamespaces %v) — it must never be backed up: %s", m, s.Include, veleroWhy(m))
			}
		}
	}
	return nil
}

func veleroWhy(ns string) string {
	if ns == "llz-openbao" {
		return "the OpenBao seal key beside the Raft store it unseals"
	}
	return "Velero's own keys, including the SSE-C key its backups are encrypted under"
}

// VeleroSchedule is the cron expression for an interval of n hours.
func VeleroSchedule(n int) string {
	if n == 24 {
		return fmt.Sprintf("%d %d * * *", veleroMinute, veleroDailyHour)
	}
	return fmt.Sprintf("%d */%d * * *", veleroMinute, n)
}

// VeleroRetentionHours converts a retention knob (the durationRe forms) to whole
// hours, which is the unit Velero's TTL (a Go duration, which has no `d`) is
// rendered in. Sub-hour units round up to an hour; ok=false for anything that is
// not a duration.
func VeleroRetentionHours(r string) (int, bool) {
	m := veleroDurationRe.FindStringSubmatch(r)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "h":
		return n, true
	case "d":
		return n * 24, true
	case "w":
		return n * 24 * 7, true
	case "y":
		return n * 24 * 365, true
	case "m":
		return (n + 59) / 60, true
	default: // s and finer
		if n == 0 {
			return 0, true
		}
		return 1, true
	}
}

// validateVeleroKnobs bounds the velero component's knobs. Called from
// validateComponentSizing, which has already rejected knobs velero does not read.
func validateVeleroKnobs(env string, t ComponentToggle) []error {
	var errs []error
	at := func(knob string) string { return fmt.Sprintf("environments.%s.components.velero.%s", env, knob) }
	interval := VeleroDefaultIntervalHours
	if t.BackupIntervalHours != nil {
		interval = *t.BackupIntervalHours
	}
	if t.Retention != "" {
		h, ok := VeleroRetentionHours(t.Retention)
		switch {
		case !ok:
			// durationRe already reported it.
		case h < 2*interval || h < 24:
			errs = append(errs, fmt.Errorf("%s %q is shorter than two backup intervals (%dh) or a day: a backup would expire before "+
				"its successor existed, leaving windows with nothing to restore", at("retention"), t.Retention, 2*interval))
		case h > VeleroMaxRetentionHours:
			errs = append(errs, fmt.Errorf("%s %q is longer than a year (%dh): every backup holds the included namespaces' Secrets, "+
				"and the versioned bucket keeps deleted ones for velero_noncurrent_days on top", at("retention"), t.Retention, VeleroMaxRetentionHours))
		}
	}
	include := strings.Fields(t.BackupIncludeNamespaces)
	for _, n := range include {
		if !veleroNSRe.MatchString(n) {
			errs = append(errs, fmt.Errorf("%s: %q is not a namespace name or a Velero glob over one", at("backupIncludeNamespaces"), n))
		}
		for _, m := range VeleroMandatoryExcludes {
			if n == m {
				errs = append(errs, fmt.Errorf("%s names %s, which is never backed up (%s) — remove it", at("backupIncludeNamespaces"), m, veleroWhy(m)))
			}
		}
	}
	if len(include) > 1 {
		for _, n := range include {
			if n == "*" {
				errs = append(errs, fmt.Errorf("%s mixes \"*\" with named namespaces (%q): Velero refuses that; use \"*\" alone, or list the namespaces",
					at("backupIncludeNamespaces"), t.BackupIncludeNamespaces))
				break
			}
		}
	}
	for _, n := range strings.Fields(t.BackupExcludeNamespaces) {
		if n == "*" {
			errs = append(errs, fmt.Errorf("%s excludes \"*\": every backup would be empty — a schedule that succeeds having saved nothing. Disable the component instead",
				at("backupExcludeNamespaces")))
			continue
		}
		if !veleroNSRe.MatchString(n) {
			errs = append(errs, fmt.Errorf("%s: %q is not a namespace name or a Velero glob over one", at("backupExcludeNamespaces"), n))
		}
	}
	return errs
}

// RenderVeleroPatch returns the per-env merge patch for the llz-velero-server
// Application: the BackupStorageLocation (this deployment's bucket on its
// object-storage endpoint, addressed virtual-hosted, SSE-C under the
// velero-ssec-key Secret) and the `cluster` Schedule (cadence, TTL and namespace
// scope from the toggle, defaults for unset knobs). Lists in a CRD merge patch
// REPLACE, so the placeholder BSL and namespace lists in the base manifest never
// survive into a rendered tree. Always emitted when the component is enabled, so
// the values the cluster runs are the ones the rendered tree shows.
func RenderVeleroPatch(objCluster, bucket string, t ComponentToggle) string {
	interval := VeleroDefaultIntervalHours
	if t.BackupIntervalHours != nil {
		interval = *t.BackupIntervalHours
	}
	retention := t.Retention
	if retention == "" {
		retention = VeleroDefaultRetention
	}
	ttl, ok := VeleroRetentionHours(retention)
	if !ok {
		ttl, _ = VeleroRetentionHours(VeleroDefaultRetention)
	}
	scope := VeleroScopeFor(t)
	var b strings.Builder
	b.WriteString("# GENERATED by `llz render` — this deployment's Velero bucket, endpoint and\n" +
		"# schedule (from spec.components.velero: backupIntervalHours, retention,\n" +
		"# backupIncludeNamespaces, backupExcludeNamespaces). llz-openbao and llz-velero\n" +
		"# are always excluded; see clusterspec.VeleroMandatoryExcludes.\n")
	b.WriteString(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ` + VeleroApplication + `
  namespace: argocd
spec:
  source:
    helm:
      valuesObject:
        configuration:
          backupStorageLocation:
            - name: default
              provider: aws
              bucket: ` + bucket + `
              default: true
              config:
                region: ` + objCluster + `
                s3Url: https://` + ObjEndpointHost(objCluster) + `
                s3ForcePathStyle: "false"
                checksumAlgorithm: ""
                customerKeyEncryptionSecret: velero-ssec-key/customer-key
        schedules:
          cluster:
            disabled: false
            schedule: "` + VeleroSchedule(interval) + `"
            useOwnerReferencesInBackup: false
            template:
              ttl: ` + strconv.Itoa(ttl) + `h
              storageLocation: default
              snapshotVolumes: false
              defaultVolumesToFsBackup: false
              includedNamespaces:
`)
	for _, n := range scope.Include {
		b.WriteString("                - " + strconv.Quote(n) + "\n")
	}
	b.WriteString("              excludedNamespaces:\n")
	for _, n := range scope.Exclude {
		b.WriteString("                - " + strconv.Quote(n) + "\n")
	}
	return b.String()
}
