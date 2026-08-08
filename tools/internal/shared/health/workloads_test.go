package health

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyWorkload(t *testing.T) {
	cases := []struct {
		name           string
		kind, ns, wl   string
		desired, ready int
		preason, pmsg  string
		phase1         bool
		want           Category
	}{
		{"scaled down", "Deployment", "x", "a", 0, 0, "", "", false, CatOK},
		{"fully ready", "Deployment", "x", "a", 3, 3, "", "", false, CatOK},
		{"phase1 cascade", "StatefulSet", "openbao", "platform-openbao", 1, 0, "", "", true, CatPending},
		{"deferred workload", "Deployment", "external-dns", "external-dns", 1, 0, "", "", false, CatDeferred},
		{"deferred suffix", "Deployment", "kube-system", "linode-internal-cidr-firewall-abc", 1, 0, "", "", false, CatDeferred},
		{"plain fail", "Deployment", "x", "app", 2, 1, "", "", false, CatFail},
		{"phase1 cascade ignored off-phase", "StatefulSet", "openbao", "platform-openbao", 1, 0, "", "", false, CatFail},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := ClassifyWorkload(c.kind, c.ns, c.wl, c.desired, c.ready, c.preason, c.pmsg, c.phase1)
			if got != c.want {
				t.Errorf("ClassifyWorkload = %v, want %v", got, c.want)
			}
		})
	}
	// ProgressDeadlineExceeded message is appended on a genuine failure.
	_, msg := ClassifyWorkload("Deployment", "x", "app", 2, 0, "ProgressDeadlineExceeded", "ReplicaSet has timed out", false)
	if want := "Deployment x/app (0/2) — Progressing=ProgressDeadlineExceeded: ReplicaSet has timed out"; msg != want {
		t.Errorf("msg = %q, want %q", msg, want)
	}
}

func TestClassifyDaemonSet(t *testing.T) {
	if cat, _ := ClassifyDaemonSet("kube-system", "csi", 3, 3, 3, 0); cat != CatOK {
		t.Error("fully rolled DS should be OK")
	}
	if cat, _ := ClassifyDaemonSet("kube-system", "csi", 3, 2, 3, 0); cat != CatFail {
		t.Error("not-ready DS should fail")
	}
	if cat, msg := ClassifyDaemonSet("kube-system", "csi", 3, 3, 2, 0); cat != CatFail || !strings.Contains(msg, "rolling update stalled") {
		t.Errorf("stalled rollout should fail: %q", msg)
	}
	if cat, msg := ClassifyDaemonSet("kube-system", "csi", 3, 3, 3, 1); cat != CatFail || !strings.Contains(msg, "misscheduled") {
		t.Errorf("misscheduled should fail: %q", msg)
	}
}

func TestClassifyPVC(t *testing.T) {
	if cat, _ := ClassifyPVC("x", "data", "Pending", "block-storage-retain"); cat != CatFail {
		t.Error("Pending PVC should fail")
	}
	if cat, _ := ClassifyPVC("x", "data", "Bound", "block-storage-retain"); cat != CatOK {
		t.Error("Bound on the right class should be OK")
	}
	// Named chart-hardcoded PVC on the linode-default class is an expected deferral.
	if cat, _ := ClassifyPVC("gitea", "valkey-data-gitea-valkey-primary-0", "Bound", "linode-block-storage"); cat != CatDeferred {
		t.Error("expected linode-block-storage PVC should be deferred")
	}
	// Unlisted PVC on the linode-default class is a warn (not a verdict failure).
	if cat, _ := ClassifyPVC("x", "rando", "Bound", "linode-block-storage"); cat != CatWarn {
		t.Error("unlisted PVC on linode-block-storage should warn")
	}
}

func TestLeaseStale(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	// dur=15 -> stale threshold 60s.
	if LeaseStale(now.Add(-50*time.Second), now, 15) {
		t.Error("50s < 60s threshold — not stale")
	}
	if !LeaseStale(now.Add(-90*time.Second), now, 15) {
		t.Error("90s > 60s threshold — stale")
	}
	// Missing/zero duration falls back to 15.
	if !LeaseStale(now.Add(-120*time.Second), now, 0) {
		t.Error("zero duration should default to 15 (threshold 60s)")
	}
}

func TestReportAddWarnIsNoop(t *testing.T) {
	var r Report
	r.Add(CatWarn, "informational")
	if len(r.Failed)+len(r.Pending)+len(r.Deferred)+len(r.Drift) != 0 {
		t.Error("CatWarn must not enter any verdict bucket")
	}
	if r.Verdict() != Converged {
		t.Error("a warn-only report is converged")
	}
}
