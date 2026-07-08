package main

// doctor_crossorg.go implements the cross-org reuse guardrail (#200): a `llz
// doctor` preflight that fails loudly when an instance workflow calls a reusable
// workflow in a DIFFERENT GitHub org while relying on `secrets: inherit`.
//
// GitHub delivers inherited secrets — repo-, org-, AND environment-scoped alike —
// as EMPTY across an org boundary (only same-org/enterprise inheritance works).
// The reusable still *runs* (repo read is granted), so there is no access error;
// every credentialed step just fails far downstream with cryptic messages
// (`No valid credential sources found`, `require-secret … is not set`). The
// failure is silent at setup time and only surfaces on the first pipeline run.
// This turns that multi-hour trap into a one-line, actionable preflight.
//
// The structural fix is the local-job-graph reuse pattern
// (docs/designs/cross-org-reuse-pattern.md); this guardrail protects every
// instance still on the thin-caller shape in the meantime.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// crossOrgReuseFinding is one job that calls a reusable workflow in an org other
// than the instance repo's while passing `secrets: inherit`.
type crossOrgReuseFinding struct {
	File    string
	Job     string
	UsesOrg string
}

// wfForReuse is the minimal slice of a workflow file this check parses.
// sigs.k8s.io/yaml routes through JSON, so the tags are json tags. `secrets` is
// `interface{}` because it is either the string "inherit" or a mapping.
type wfForReuse struct {
	Jobs map[string]struct {
		Uses    string      `json:"uses"`
		Secrets interface{} `json:"secrets"`
	} `json:"jobs"`
}

// usesOrg returns the GitHub owner a `uses:` reference points at, or "" for a
// local (`./…`) reference, a docker reference, or a malformed value.
func usesOrg(uses string) string {
	uses = strings.TrimSpace(uses)
	if uses == "" || strings.HasPrefix(uses, "./") || strings.HasPrefix(uses, "docker://") {
		return ""
	}
	if i := strings.IndexByte(uses, '@'); i >= 0 {
		uses = uses[:i] // drop @ref
	}
	owner, _, ok := strings.Cut(uses, "/")
	if !ok {
		return ""
	}
	return owner
}

// crossOrgSecretInheritFindings parses one workflow's YAML and reports each job
// that calls a reusable workflow in an org other than repoOwner while passing
// `secrets: inherit`. Org comparison is case-insensitive (GitHub owners are). A
// `uses:` still carrying a copier token (`<@ … @>`) is the un-rendered template,
// not an instance, and is skipped.
func crossOrgSecretInheritFindings(content, repoOwner, file string) ([]crossOrgReuseFinding, error) {
	var wf wfForReuse
	if err := yaml.Unmarshal([]byte(content), &wf); err != nil {
		return nil, err
	}
	var out []crossOrgReuseFinding
	for job, j := range wf.Jobs {
		if j.Uses == "" || strings.Contains(j.Uses, "<@") {
			continue
		}
		if s, ok := j.Secrets.(string); !ok || strings.TrimSpace(s) != "inherit" {
			continue // no `secrets: inherit` on this job (explicit mapping or none)
		}
		org := usesOrg(j.Uses)
		if org == "" || strings.EqualFold(org, repoOwner) {
			continue
		}
		out = append(out, crossOrgReuseFinding{File: file, Job: job, UsesOrg: org})
	}
	return out, nil
}

// checkCrossOrgReuse is the doctor section. It no-ops (with a pass line) outside
// an instance or when the instance's own org is unknown — there is nothing to
// compare against there. Returns a non-nil error when any cross-org
// `secrets: inherit` job is found, so doctor's overall gate fails.
func checkCrossOrgReuse() error {
	a, _ := readAnswers(".")
	if a == nil || a.InstanceRepo == "" {
		report("workflow reuse (no instance repo to check)", true)
		return nil
	}
	owner, _, ok := strings.Cut(a.InstanceRepo, "/")
	if !ok || owner == "" {
		report("workflow reuse (instance_repo has no owner)", true)
		return nil
	}

	var files []string
	for _, pat := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml"} {
		if m, err := filepath.Glob(pat); err == nil {
			files = append(files, m...)
		}
	}
	sort.Strings(files)

	var findings []crossOrgReuseFinding
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		// A malformed workflow is actionlint's job, not this gate — skip on parse error.
		fs, err := crossOrgSecretInheritFindings(string(b), owner, filepath.ToSlash(f))
		if err != nil {
			continue
		}
		findings = append(findings, fs...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Job < findings[j].Job
	})

	if len(findings) == 0 {
		report("no cross-org `secrets: inherit` reuse", true)
		return nil
	}
	report("cross-org `secrets: inherit` reuse", false)
	for _, f := range findings {
		fmt.Printf("      %s (job %q): uses org %q ≠ repo org %q\n", f.File, f.Job, f.UsesOrg, owner)
	}
	fmt.Println("      `secrets: inherit` does NOT cross organizations — the pipeline will run with EMPTY")
	fmt.Println("      secrets (repo, org, and environment scoped). Repoint `uses:` to a same-org fork, or")
	fmt.Println("      adopt the local-job-graph reuse pattern (docs/designs/cross-org-reuse-pattern.md).")
	return fmt.Errorf("cross-org `secrets: inherit` in %d workflow job(s)", len(findings))
}
