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
