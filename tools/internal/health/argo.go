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
	OpErr     string // .status.operationState.message when the last sync FAILED (apply-time errors, e.g. the 256KB annotation limit — these never surface as a ComparisonError)
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
	// A sync that failed on the 256KB metadata.annotations limit ("... is invalid:
	// metadata.annotations: Too long") is an infra wedge, not an app-config fault:
	// a CRD (or other object) carries an oversized client-side last-applied-
	// configuration annotation, so every apply to it fails. The convergence gate
	// self-heals by stripping that annotation and re-polling (see runConverge), so
	// treat it as transient/pending rather than a hard strike — same rationale as
	// the argocd-redis auth split above.
	if IsAnnotationLimitError(a.OpErr) {
		return CatPending, label + " — sync hit the 256KB annotation limit; converge strips the oversized CRD annotation and re-polls"
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
		// A GIT auth refusal, by contrast, is terminal. Argo is being told "no" by
		// the remote for the credential it holds; polling cannot change the answer,
		// and nothing in the bootstrap re-mints that credential. Saying so here —
		// rather than letting it ride as a generic CatFail that phase1 then
		// downgrades to in-progress — is what stops the gate from spending its
		// entire budget on a question already answered. Checked AFTER the Redis case
		// because Redis's NOAUTH message is literally "NOAUTH Authentication
		// required." and would otherwise match.
		if IsGitAuthError(a.SpecErr) {
			return CatFail, label + " — " + a.SpecErr +
				"  ⇒ TERMINAL: Argo CD cannot authenticate to the source repo. Polling will not fix this. " +
				"Check the values-repo credential (APL_VALUES_REPO_TOKEN → otomi.git.password → the argocd " +
				"repo Secret), and that the repo Secret actually materialized — it arrives via an ExternalSecret, " +
				"so it stays empty if external-secrets never installed."
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

// IsGitAuthError reports whether a ComparisonError message is the git remote
// refusing Argo's credential — as opposed to a network flake, a manifest fault,
// or the Redis cache split above.
//
// The distinction is the whole point. gsap-apl run 29709276389 spent its entire
// 1200s convergence budget polling this:
//
//	gitops-global (Unknown/Healthy) — ComparisonError: failed to list refs:
//	  authentication required: Unauthorized
//
// Nothing about that resolves by waiting. The remote answered, the answer was
// "no", and it will keep being "no" until an operator fixes the credential. Every
// other failure in that run — external-secrets CRDs absent, apl-sops-secrets
// missing, apl-operator in CreateContainerConfigError, Harbor's registry down —
// was downstream of it, so 20 minutes of polling bought nothing but a longer log.
//
// Deliberately NOT matched: "repository not found" (a private repo returns it to
// a credential that cannot see it, but so does a genuinely wrong repoURL, and the
// two are indistinguishable from the message alone) and bare "403" (Argo wraps
// several unrelated upstream 403s). Both stay generic failures. Callers must test
// IsRepoServerCacheAuthError FIRST — Redis's NOAUTH text contains "authentication
// required" and is transient, the exact opposite verdict.
func IsGitAuthError(specErr string) bool {
	if specErr == "" || IsRepoServerCacheAuthError(specErr) {
		return false
	}
	m := strings.ToLower(specErr)
	for _, p := range []string{
		"authentication required",                // go-git's 401 on ref discovery
		"invalid username or password",           // git-over-HTTPS credential rejection
		"authentication failed",                  // generic transport refusal
		"could not read username",                // no credential at all, prompts disabled
		"terminal prompts disabled",              // ditto
		"ssh: handshake failed",                  // key-based remote rejecting the key
		"permission denied (publickey",           // ditto
		"write access to repository not granted", // valid token, insufficient scope
	} {
		if strings.Contains(m, p) {
			return true
		}
	}
	return false
}

// IsAnnotationLimitError reports whether a sync/operation error message carries
// the Kubernetes 256KB metadata.annotations cap ("metadata.annotations: Too
// long"). It's raised when applying an object whose client-side last-applied-
// configuration annotation is oversized — most often a CRD with a huge embedded
// OpenAPI schema (Kyverno policy CRDs, Gateway-API httproutes). The convergence
// gate strips that annotation to unwedge the apply, so this marks a self-healable
// transient rather than a real spec fault.
func IsAnnotationLimitError(msg string) bool {
	return strings.Contains(msg, "annotations: Too long")
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
		OperationState struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"operationState"`
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
	// A FAILED sync surfaces its apply-time error in operationState.message (never
	// as a ComparisonError). Only carry it when the phase actually failed, so a
	// Succeeded/Running message can't be mistaken for an error.
	opErr := ""
	if j.Status.OperationState.Phase == "Failed" || j.Status.OperationState.Phase == "Error" {
		opErr = j.Status.OperationState.Message
	}
	return ArgoApp{
		Name:      j.Metadata.Name,
		Sync:      j.Status.Sync.Status,
		Health:    j.Status.Health.Status,
		Automated: auto,
		SpecErr:   strings.Join(specErrs, " | "),
		OpErr:     opErr,
	}, nil
}
