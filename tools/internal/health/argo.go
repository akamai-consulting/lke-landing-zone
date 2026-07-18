package health

import (
	"encoding/json"
	"fmt"
	"strings"
)

// argo.go ports section 2 (ArgoCD Applications) — the script's most intricate
// classification: the order in which an Application's sync/health/automated/
// spec-error state, the operator-deferred allowlist, and the Phase-1 cascade
// resolve to pass/fail/pending/deferred/drift.

// ArgoApp is the extracted status of one ArgoCD Application.
type ArgoApp struct {
	Name      string
	Sync      string // .status.sync.status
	Health    string // .status.health.status
	Automated bool   // .spec.syncPolicy.automated present (non-null)
	SpecErr   string // joined ComparisonError/InvalidSpecError condition messages
}

// ClassifyArgoApp applies section 2's decision order to one Application:
//  1. operator-deferred (EXTERNAL_DEP_APPS) wins even over a spec error, since
//     some deferrals surface AS a ComparisonError (e.g. an operator-supplied token missing);
//  2. a persistent ComparisonError/InvalidSpecError is a real failure;
//  3. Synced+Healthy passes;
//  4. no automated syncPolicy => intentionally hand-managed => pending;
//  5. Phase-1 cascade apps waiting on OpenBao => pending;
//  6. OutOfSync but Healthy => cosmetic drift;
//  7. Progressing => reconcile still in flight => pending (poll, don't fail);
//  8. otherwise => fail.
func ClassifyArgoApp(a ArgoApp, phase1 bool) (Category, string) {
	label := fmt.Sprintf("%s (%s/%s)", a.Name, a.Sync, a.Health)
	if reason, ok := MatchExternalDep(a.Name, ExternalDepApps()); ok {
		return CatDeferred, label + " — " + reason
	}
	if a.SpecErr != "" {
		// A ComparisonError carrying a Redis auth code (WRONGPASS/NOAUTH) is not an
		// app-config fault: it is the ArgoCD repo-server failing to authenticate to
		// its argocd-redis manifest cache, so *every* Application ComparisonErrors
		// at once. That happens when the argocd-redis password rotates under a
		// never-restarted redis pod (a reused-cluster e2e can leave redis holding a
		// stale --requirepass while freshly-rolled repo-servers read the new secret).
		// A `rollout restart deploy/argocd-redis` realigns them, so this is a
		// transient infra condition to POLL on — same treatment as a Progressing
		// rollout — not a hard strike. A genuine per-app spec error never carries a
		// Redis auth code, so the reclassification cannot mask a real failure. The
		// convergence gate now self-heals this by restarting argocd-redis once when
		// it observes the split persisting across polls (see runConverge); worst
		// case (restart ineffective) the gate simply exhausts its budget and exits 1.
		if IsRepoServerCacheAuthError(a.SpecErr) {
			return CatPending, label + " — argocd-redis cache auth (repo-server↔redis password split); transient, polling"
		}
		return CatFail, label + " — " + a.SpecErr
	}
	if a.Sync == "Synced" && a.Health == "Healthy" {
		return CatOK, label
	}
	if !a.Automated {
		return CatPending, label + " — sync suspended (no automated policy)"
	}
	if phase1 && MatchPrefix(a.Name, Phase1PendingApps()) {
		return CatPending, label + " — waiting on OpenBao bootstrap"
	}
	if a.Health == "Healthy" {
		return CatDrift, label + " — drift only; workload functional"
	}
	// Progressing is ArgoCD's "reconcile still in flight" health — a Deployment
	// rolling out, a child resource not yet Ready (e.g. loki-gateway waiting on
	// its loki backend). That is the canonical in-progress state the convergence
	// gate must POLL on, not hard-fail. A genuinely stuck rollout flips to
	// Degraded (caught below) once progressDeadlineSeconds lapses, and the gate's
	// overall budget bounds the poll — so treating it as pending cannot hang.
	if a.Health == "Progressing" {
		return CatPending, label + " — rolling out (Progressing)"
	}
	return CatFail, label
}

// IsRepoServerCacheAuthError reports whether a ComparisonError message carries a
// Redis authentication code. The ArgoCD repo-server caches git refs/manifests in
// argocd-redis; when its AUTH fails (e.g. the redis password rotates under a
// never-restarted redis pod), ListRefs returns "failed to list refs: WRONGPASS
// ..." and the Application goes Unknown/ComparisonError. WRONGPASS ("invalid
// username-password pair") and NOAUTH ("authentication required") originate only
// in the redis cache path, never in an Application's own source, so they mark a
// transient infra blip the convergence gate should poll on rather than a real
// spec fault.
func IsRepoServerCacheAuthError(specErr string) bool {
	return strings.Contains(specErr, "WRONGPASS") || strings.Contains(specErr, "NOAUTH")
}

// argoAppJSON is the subset of an ArgoCD Application object we parse.
type argoAppJSON struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		SyncPolicy struct {
			Automated json.RawMessage `json:"automated"`
		} `json:"syncPolicy"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status string `json:"status"`
		} `json:"sync"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		Conditions []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// ParseArgoApp extracts an ArgoApp from one Application's JSON, joining any
// ComparisonError/InvalidSpecError condition messages (the authoritative
// "spec can't render" signal) and resolving the automated-syncPolicy flag —
// the same fields the APP_OUT jq pulls.
func ParseArgoApp(raw []byte) (ArgoApp, error) {
	var j argoAppJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return ArgoApp{}, err
	}
	var specErrs []string
	for _, c := range j.Status.Conditions {
		if c.Type == "ComparisonError" || c.Type == "InvalidSpecError" {
			msg := c.Message
			if msg == "" {
				msg = "(no message)"
			}
			specErrs = append(specErrs, c.Type+": "+msg)
		}
	}
	auto := len(j.Spec.SyncPolicy.Automated) > 0 && strings.TrimSpace(string(j.Spec.SyncPolicy.Automated)) != "null"
	return ArgoApp{
		Name:      j.Metadata.Name,
		Sync:      j.Status.Sync.Status,
		Health:    j.Status.Health.Status,
		Automated: auto,
		SpecErr:   strings.Join(specErrs, " | "),
	}, nil
}
