package main

// ci_cosign_subject_guard.go implements `llz ci cosign-subject-guard` — assert
// that every workflow file named in a cosign keyless `subject:` still exists.
//
// Why it exists: keyless signing derives the certificate subject from the
// signing workflow's PATH. kyverno-verify-llz-image-signature.yaml pins
//
//	https://github.com/akamai-consulting/*/.github/workflows/build-images.yml@*
//
// so the policy only trusts signatures produced by a workflow at exactly that
// path. Renaming or moving build-images.yml changes the subject of every
// signature it produces afterwards, the glob stops matching, and verifyImages
// starts REJECTING the very images we just built and signed.
//
// That failure is loud in the wrong place and quiet in the right one. Nothing in
// template-repo CI notices — the rename is green here — and the breakage lands
// later, in clusters, as pods that never get admitted. Four workloads run the
// gated image: the llz-reconciler Deployment (every watch lane), the
// broad-pat-rotator and harbor-robot-provisioner CronJobs, and the
// cluster-health WorkflowTemplate. A Deployment whose pods fail admission does
// not crash-loop or page; the ReplicaSet just records events nobody reads,
// credential rotation stops, and the cluster looks fine until something needs
// the thing that quietly stopped running.
//
// The guard is the cheap half of that trade: a rename becomes a red PR in the
// repo doing the renaming, with the policy file named, instead of a silent
// admission failure in every instance downstream.
//
// SCOPE: this checks that the pinned path RESOLVES. It deliberately does not
// judge how tightly the subject is scoped — the org-wildcard in the repo
// position is a documented trust-anchor decision in the policy's own comments,
// and narrowing it is the owner's call, not this guard's.
//
// The extraction is pure and unit-tested; the filesystem is reached only by the
// walk in RunE.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// reCosignWorkflowSubject matches a keyless `subject:` whose identity is a
// GitHub Actions workflow, capturing the workflow's basename.
//
// Tolerates quoted and unquoted YAML scalars, and any org/repo shape (including
// the `*` glob the live policy uses) — the org and ref are not this guard's
// business, only the workflow path is.
var reCosignWorkflowSubject = regexp.MustCompile(
	`(?m)^\s*subject:\s*["']?(https://github\.com/[^"'\s]*?/\.github/workflows/([A-Za-z0-9._-]+\.ya?ml))@[^"'\s]*["']?\s*$`)

// cosignSubjectRef is one workflow path pinned by one policy file.
type cosignSubjectRef struct {
	File     string // repo-relative policy file
	Subject  string // the full subject up to the @ref
	Workflow string // basename, e.g. build-images.yml
}

// extractCosignSubjects returns every workflow-identity subject in a manifest.
func extractCosignSubjects(body string) []cosignSubjectRef {
	var out []cosignSubjectRef
	for _, m := range reCosignWorkflowSubject.FindAllStringSubmatch(body, -1) {
		out = append(out, cosignSubjectRef{Subject: m[1], Workflow: m[2]})
	}
	return out
}

func ciCosignSubjectGuardCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "cosign-subject-guard",
		Short: "fail when a cosign keyless subject names a workflow that no longer exists",
		Long: "Scans the platform manifest trees for Kyverno keyless `subject:` pins that\n" +
			"identify a GitHub Actions workflow, and fails if the named workflow file is\n" +
			"missing from .github/workflows/. Keyless signing derives the certificate\n" +
			"subject from the workflow's path, so renaming the signing workflow silently\n" +
			"invalidates every signature the policy will accept — surfacing not here, but\n" +
			"as pods that fail admission in every downstream cluster.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCosignSubjectGuard(root)
		},
	}
	c.Flags().StringVar(&root, "root", ".", "repository root")
	return c
}

func runCosignSubjectGuard(root string) error {
	var refs []cosignSubjectRef

	dirs := platformTreeDirs(root)
	examined, err := walkManifests(dirs, func(path string, b []byte) error {
		found := extractCosignSubjects(string(b))
		if len(found) == 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		for _, f := range found {
			f.File = rel
			refs = append(refs, f)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := requireCorpus("cosign-subject-guard", examined, dirs); err != nil {
		return err
	}

	// An empty result is NOT a pass. This guard exists because a path pin can go
	// stale silently; a guard that finds no pins at all has the same blind spot
	// one directory up — the policy itself could have been renamed or moved out
	// of the scanned tree, and "zero pins, all valid" would report green.
	if len(refs) == 0 {
		return fmt.Errorf("cosign-subject-guard: found no keyless workflow `subject:` pins under %s, "+
			"but the repo signs its first-party image keyless and gates it at admission. "+
			"Either the signature policy moved out of the scanned tree or the subject format changed — "+
			"both leave this guard checking nothing. Update the guard's roots or its matcher",
			strings.Join(dirs, ", "))
	}

	var missing []cosignSubjectRef
	for _, r := range refs {
		if _, statErr := os.Stat(filepath.Join(root, ".github", "workflows", r.Workflow)); statErr != nil {
			missing = append(missing, r)
		}
	}
	if len(missing) == 0 {
		for _, r := range refs {
			fmt.Printf("  ok: %s → .github/workflows/%s\n", r.File, r.Workflow)
		}
		fmt.Printf("All %d cosign workflow subject(s) resolve to a workflow that exists.\n", len(refs))
		return nil
	}

	sort.Slice(missing, func(i, j int) bool { return missing[i].File < missing[j].File })
	var b strings.Builder
	fmt.Fprintf(&b, "cosign-subject-guard: %d keyless subject pin(s) name a workflow that does not exist:\n", len(missing))
	for _, m := range missing {
		fmt.Fprintf(&b, "  %s\n    subject: %s\n    missing: .github/workflows/%s\n", m.File, m.Subject, m.Workflow)
	}
	b.WriteString("\nKeyless signing derives the certificate subject from the signing workflow's path, so a\n" +
		"renamed or moved workflow produces signatures this policy will not match. Admission then\n" +
		"REJECTS the freshly built image — including the llz-reconciler Deployment, whose pods simply\n" +
		"never get created. Either restore the workflow path or update the subject pin to the new one\n" +
		"(and re-sign, or re-publish, any image whose signature carries the old subject).")
	return fmt.Errorf("%s", b.String())
}
