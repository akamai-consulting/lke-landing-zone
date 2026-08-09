package capability_test

// AN UNPOLICED LINODE CLIENT IS THE HOLE THE CLOUD GRANTS CANNOT SEE. linode's
// MethodPolicy defaults to nil-permits-everything, which is right for
// internal/verbs and cmd/llz — they declare no bindings and there is no grant to
// check against — and wrong for anything under internal/extensions, where the
// declaration exists and is supposed to mean something.
//
// So this counts direct constructions per package and fails when a new one
// appears. Like the raw-kubectl and raw-read ratchets it fails in BOTH
// directions: a converted package must be deleted from the list, so the paydown
// is banked rather than left as room to regrow.
//
// ────────────────────────────────────────────────────────────────────────────
// NOTHING IS LEFT. Every Linode client under internal/extensions is now built
// through capability.CloudFor, so this list is empty and an entry appearing in it
// is a regression rather than a status report.
//
// ────────────────────────────────────────────────────────────────────────────
// THE LAST ONE NEEDED A DECISION, AND THE REASON RECORDED HERE FOR DEFERRING IT
// WAS WRONG ON THE FACTS. It is corrected rather than deleted, because the wrong
// version was repeated into two commit messages and is the kind of thing that
// gets believed twice.
//
// The claim was that teardown's reapers "serve both bindings through one
// function" — that `runCIReapVolumes(…, requireEmpty)` reaps when told to and
// asserts when told to, so no call site could choose. Reading the code:
//
//   - `--require-empty` adds a verification pass AFTER the sweep. It never
//     suppresses a delete. What gates deletion is `--yes`/`--dry-run`.
//   - the assertion has its OWN entry point. RunAssertNoOrphans takes its client
//     from a Deps seam, and its interface (OrphanGateScanner) exposes no delete
//     method at all. It was never one of the six construction sites.
//
// So the six were all transition-side, and the only genuinely runtime-varying
// question was whether a given reap run deletes at all. Two sites turned out to
// be statically read-only (RunCapture lists; RunDestroyUnwedge reads a kubeconfig
// so it can act inside the cluster), two statically mutating (RunForceDelete,
// RunDeleteVPC), and four flag-gated.
//
// THE FLAG-GATED FOUR SET A PRECEDENT: a binding chosen at runtime. It is bounded
// by a rule — selection may only NARROW — so the maximum a code path may do stays
// static and readable from the declaration, and TestTheRuntimeSelectionOnlyEverNarrows
// pins it. What it buys is specific: `--dry-run` not deleting was enforced by a
// single early `return` inside Deleter's closure, and there is now an independent
// refusal at the transport behind it.
// ────────────────────────────────────────────────────────────────────────────

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// allowedRawCloud is every package under internal/extensions still constructing a
// Linode client without a policy, with why.
//
// The reason is the same for all nine and is recorded once here rather than
// repeated: cloud-read and cloud-mutate sit on different bindings, so the call
// site needs a judgement about WHICH, and several serve both modes from one
// function.
var allowedRawCloud = map[string]int{}

var rawCloudCtor = regexp.MustCompile(`\blinode\.(NewClient|ClientFromEnv)\(`)

func TestNoNewUnpolicedLinodeClients(t *testing.T) {
	root := filepath.FromSlash("../../extensions")
	got := map[string]int{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Comments do not count: several headers narrate the conversion, and a
		// ratchet that failed on a documentation change is one people delete.
		n := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			n += len(rawCloudCtor.FindAllString(line, -1))
		}
		if n > 0 {
			rel, _ := filepath.Rel(root, path)
			seg := strings.Split(filepath.ToSlash(rel), "/")
			got[seg[len(seg)-2]] += n
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	for pkg, n := range got {
		want, ok := allowedRawCloud[pkg]
		if !ok {
			t.Errorf("%s builds a Linode client with no policy (%d site(s)) — its declared cloud "+
				"grants therefore constrain nothing, and cloud-mutate is the grant that deletes "+
				"clusters. Build it from capability.CloudFor(<binding>).Client(...), or add it here "+
				"with the reason it cannot be.", pkg, n)
			continue
		}
		if n > want {
			t.Errorf("%s: %d unpoliced Linode clients, allowed %d — a NEW one appeared. Route it "+
				"through capability.CloudFor rather than growing this list.", pkg, n, want)
		}
		if n < want {
			t.Errorf("%s: %d unpoliced Linode clients but %d allowed — LOWER IT to %d in this "+
				"commit, so the paydown is banked instead of left as room to regrow", pkg, n, want, n)
		}
	}
	var gone []string
	for pkg := range allowedRawCloud {
		if _, still := got[pkg]; !still {
			gone = append(gone, pkg)
		}
	}
	sort.Strings(gone)
	if len(gone) > 0 {
		t.Errorf("these packages no longer build an unpoliced Linode client — delete them from "+
			"allowedRawCloud: %s", strings.Join(gone, ", "))
	}
}
