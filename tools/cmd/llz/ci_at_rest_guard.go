package main

// ci_at_rest_guard.go implements `llz ci at-rest-guard` — the static gate on
// ENCRYPTION AT REST for the things this repo declares in Terraform.
//
// WHAT IT IS FOR. The at-rest posture of an instance rests on three levers, each
// spelled once per resource and each silently absent by default:
//
//   terraform { encryption }   the state and plan files. State holds
//                              `kubeconfig_raw` (cluster-admin) and every Managed
//                              Postgres `root_password` in the clear — `sensitive`
//                              controls CLI DISPLAY, never what is written — so a
//                              root without this block hands all of it to whoever
//                              holds the bucket key (ADR 0007).
//   disk_encryption            the node pool's boot and data disks: every image
//                              layer, every emptyDir, and the kubelet's on-disk
//                              copy of every Secret projected into a pod.
//   encryption (linode_volume) a directly-declared Volume. The CSI-provisioned
//                              ones are covered at runtime by
//                              `assert-volume-encryption`, which reads the Linode
//                              API; nothing covered a Volume declared in HCL.
//
// The pattern this repeats is the one PVC encryption already taught, expensively:
// encryption is decided at CREATE and is immutable afterwards. There is no
// remediation for a state file that was written unencrypted or a node pool that
// came up without disk encryption — only a rebuild. A gate is the only useful
// place to catch it, and it has to run before apply, not after.
//
// LATENT, NOT LIVE. Every rule here is green on the tree today. That is the
// condition under which a drift gate is worth writing rather than a reason to
// skip it: `llz env add` scaffolds Terraform roots, so a fifth root is a routine
// change, and adding one without encryption.tf is a one-file omission that would
// have passed every existing gate — including `tf-validate`, `checkov` and
// `tflint`, which have no opinion about a block that is simply not there.
//
// THE ONE RESIDUE IS REGISTERED, NOT IGNORED. ADR 0007 shipped its migration in
// two phases and only phase 1 has happened: all four roots still carry
// `fallback { method = method.unencrypted.migrate }`, which is what lets OpenTofu
// READ pre-encryption state. It also means an unencrypted state file is still
// accepted rather than refused. That is a deliberate, documented position — and
// it was a comment repeated in four files with no owner and no exit condition,
// which is how a two-phase migration becomes a one-phase one. Registering it puts
// the debt in one place with the test for retiring it.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// atRestRule is why one at-rest gap is allowed to stay open.
type atRestRule struct {
	// reason must say what is exposed and to whom, not merely that it is allowed.
	reason string
	// exit names the condition that RETIRES this entry. An accepted residue with
	// no exit test is a permanent one wearing a temporary label.
	exit string
}

// atRestAllowed registers every accepted at-rest gap, keyed by
// "<file-relative-path>:<locator>".
var atRestAllowed = map[string]atRestRule{
	// ── ADR 0007 phase 1: the unencrypted fallback ───────────────────────────
	//
	// One entry per root rather than one shared entry, deliberately: retiring this
	// is per-root work (each root's state has to have been migrated before its
	// fallback can go), and a single entry would let the first root that migrated
	// vouch for the three that had not.
	"tools/internal/tfroots/roots/cluster/encryption.tf:unencrypted-fallback": {
		reason: "ADR 0007 PHASE 1. `enforced` and an UNENCRYPTED fallback are mutually exclusive, " +
			"so an existing instance cannot go straight to enforced — the fallback is what reads " +
			"pre-encryption state and rewrites it encrypted. While it is here, a state file that " +
			"was written in plaintext is ACCEPTED rather than refused, and this root's state holds " +
			"kubeconfig_raw: cluster-admin on the deployment, readable by anyone with the bucket key",
		exit: "every deployment's cluster state has been migrated (a no-op apply does it); then " +
			"delete the fallback block and set `enforced = true` in its place. terraform-init " +
			"already declares method.unencrypted.migrate on its side, so phase 2 needs no change " +
			"to the action — only to this file",
	},
	"tools/internal/tfroots/roots/databases/encryption.tf:unencrypted-fallback": {
		reason: "ADR 0007 PHASE 1, as cluster/. This root's state is the sharpest of the four: " +
			"`root_password` is a provider-COMPUTED attribute on every Managed Postgres cluster, " +
			"so there is no way to keep it out of state at all — encryption is the only control",
		exit: "as cluster/ — migrate this root's state in every deployment, then swap the fallback " +
			"for `enforced = true`",
	},
	"tools/internal/tfroots/roots/object-storage/encryption.tf:unencrypted-fallback": {
		reason: "ADR 0007 PHASE 1, as cluster/. This root's state carries OBJ key material — " +
			"including, historically, the keys that reach the Loki and Harbor buckets",
		exit: "as cluster/ — migrate this root's state in every deployment, then swap the fallback " +
			"for `enforced = true`",
	},
	"tools/internal/tfroots/roots/vpc/encryption.tf:unencrypted-fallback": {
		reason: "ADR 0007 PHASE 1, as cluster/. The lowest-value state of the four (network " +
			"topology, no credential), and it moves with the others rather than separately: a " +
			"half-migrated instance split across two postures is worse to reason about than either " +
			"posture applied uniformly",
		exit: "as cluster/ — migrate this root's state in every deployment, then swap the fallback " +
			"for `enforced = true`",
	},
}

// ── the levers, and how they are spelled ────────────────────────────────────
//
// Patterns are tolerant of HCL's spelling freedom for the same reason the
// plaintext guard's are: a gate a reviewer clears by adding quotes is not a gate.
var (
	// A root is a directory that configures a backend. `terraform { backend "s3" }`
	// is the only form this repo uses, but the match is on the keyword so a
	// different backend type still registers as a root.
	reTFBackend = regexp.MustCompile(`(?m)^\s*backend\s+"[a-z0-9_]+"\s*\{`)
	// The posture block ADR 0007 puts in code, deliberately separate from the key
	// material in TF_ENCRYPTION. Its PRESENCE is what makes a hand-run apply
	// without TF_ENCRYPTION fail instead of silently writing plaintext.
	reTFEncryption = regexp.MustCompile(`(?m)^\s*encryption\s*\{`)
	// `method.unencrypted.<name>` referenced anywhere in an encryption block.
	reUnencrypted = regexp.MustCompile(`method\.unencrypted\.`)

	reResourceHead  = regexp.MustCompile(`^\s*resource\s+"([a-z0-9_]+)"\s+"([A-Za-z0-9_-]+)"\s*\{`)
	reDiskEncrypted = regexp.MustCompile(`(?i)^\s*disk_encryption\s*=\s*"enabled"\s*$`)
	reVolEncrypted  = regexp.MustCompile(`(?i)^\s*encryption\s*=\s*"enabled"\s*$`)
)

// atRestResourceLevers maps a resource type to the argument that decides its
// at-rest encryption, and to the matcher for the value that ENABLES it. Both
// Linode APIs spell the enabled value "enabled" and treat the argument as
// ForceNew — set at create or never.
var atRestResourceLevers = map[string]struct {
	arg string
	ok  *regexp.Regexp
}{
	"linode_lke_node_pool": {arg: "disk_encryption", ok: reDiskEncrypted},
	"linode_instance":      {arg: "disk_encryption", ok: reDiskEncrypted},
	"linode_volume":        {arg: "encryption", ok: reVolEncrypted},
}

type atRestFinding struct {
	key, file, what string
	line            int
	// registrable is false for findings that must be FIXED rather than accepted:
	// a missing `disk_encryption` has no defensible reason, only an unmade
	// decision, so the registry deliberately offers it no escape.
	registrable bool
}

func ciAtRestGuardCmd() *cobra.Command {
	var root string
	c := &cobra.Command{
		Use:   "at-rest-guard",
		Short: "fail when a Terraform-declared resource is not encrypted at rest",
		Long: "Static gate on encryption at rest (docs/adr/0007-terraform-state-encryption.md).\n" +
			"Asserts that every Terraform ROOT declares a `terraform { encryption }` block —\n" +
			"state holds kubeconfig_raw and every Managed Postgres root_password in the\n" +
			"clear — and that every linode_lke_node_pool / linode_instance sets\n" +
			"disk_encryption and every linode_volume sets encryption.\n\n" +
			"Encryption is decided at CREATE and is immutable afterwards, so there is no\n" +
			"remediation for a resource that came up unencrypted — only a rebuild. That is\n" +
			"why this is a pre-apply gate and not a report.\n\n" +
			"The ADR 0007 phase-1 unencrypted fallback is registered in atRestAllowed with\n" +
			"an exit condition; unregistered gaps fail, and so do registry entries whose\n" +
			"gap is gone. A missing disk_encryption cannot be registered at all.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return runCIAtRestGuard(root) },
	}
	c.Flags().StringVar(&root, "root", ".", "repo root (template or instance layout)")
	return c
}

func runCIAtRestGuard(root string) error {
	dirs := atRestScanDirs(root)
	findings, examined, err := collectAtRestFindings(root, dirs)
	if err != nil {
		return err
	}
	if err := requireCorpus("at-rest-guard", examined, dirs); err != nil {
		return err
	}

	seen := map[string]bool{}
	failed := false
	for _, f := range findings {
		rule, ok := atRestAllowed[f.key]
		if f.registrable {
			seen[f.key] = true
		}
		if ok && f.registrable {
			fmt.Printf("  ok: %s:%d %s — %s (exit: %s)\n", f.file, f.line, f.what, rule.reason, rule.exit)
			continue
		}
		failed = true
		if !f.registrable {
			fmt.Printf("::error file=%s,line=%d::%s. This one has no registry escape: the argument is "+
				"ForceNew and there is no reason to declare a resource unencrypted, only an unmade "+
				"decision. Set it.\n", f.file, f.line, f.what)
			continue
		}
		fmt.Printf("::error file=%s,line=%d::%s. Either close it, or register %q in atRestAllowed "+
			"(ci_at_rest_guard.go) with a reason naming WHAT is exposed and an exit condition that "+
			"retires the entry. See docs/adr/0007-terraform-state-encryption.md.\n",
			f.file, f.line, f.what, f.key)
	}

	var stale []string
	for k := range atRestAllowed {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		failed = true
		fmt.Printf("::error::atRestAllowed entry %q matches nothing in the tree. The gap was closed "+
			"or moved — delete the entry. A registry that keeps dead entries stops being reviewable, "+
			"because a reader cannot tell which lines are load-bearing.\n", k)
	}

	if failed {
		return fmt.Errorf("at-rest-guard: unencrypted resource(s) and/or stale registry entries")
	}
	fmt.Printf("at-rest-guard: %d file(s) examined, %d registered at-rest residual(s).\n", examined, len(findings))
	return nil
}

// atRestScanDirs are the trees this repo AUTHORS Terraform in: the embedded root
// templates every instance is scaffolded from, and the published modules they
// call. Resolved through esRepoPath for the same reason the sibling guards do —
// a raw join silently resolves to a non-existent path in the instance layout, and
// a missing tree is tolerated by the walk, so the guard would scan less than it
// appears to.
func atRestScanDirs(root string) []string {
	return []string{
		esRepoPath(root, filepath.Join("tools", "internal", "tfroots", "roots")),
		esRepoPath(root, "terraform-modules"),
	}
}

func collectAtRestFindings(root string, dirs []string) ([]atRestFinding, int, error) {
	var out []atRestFinding
	examined := 0
	// Directories that configure a backend, and whether they declare encryption.
	rootHasEncryption := map[string]bool{}
	rootBackendFile := map[string]string{}

	for _, dir := range dirs {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // a missing tree is requireCorpus's problem
			}
			if info.IsDir() {
				if b := info.Name(); b == ".terraform" || b == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".tf" {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			examined++
			rel := relFromRoot(root, path)
			body := string(b)
			dirKey := filepath.Dir(path)

			if reTFBackend.MatchString(body) {
				if _, ok := rootBackendFile[dirKey]; !ok {
					rootBackendFile[dirKey] = rel
				}
			}
			if reTFEncryption.MatchString(body) {
				rootHasEncryption[dirKey] = true
			}
			if line, ok := firstMatchLine(body, reUnencrypted); ok {
				out = append(out, atRestFinding{
					key:  rel + ":unencrypted-fallback",
					file: rel, line: line, registrable: true,
					what: "state/plan encryption accepts `method.unencrypted` — an unencrypted state " +
						"file is read rather than refused",
				})
			}
			out = append(out, scanResourceLevers(body, rel)...)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}

	// A root with a backend and no encryption block writes plaintext state.
	var missing []string
	for dirKey := range rootBackendFile {
		if !rootHasEncryption[dirKey] {
			missing = append(missing, dirKey)
		}
	}
	sort.Strings(missing)
	for _, dirKey := range missing {
		out = append(out, atRestFinding{
			key:  rootBackendFile[dirKey] + ":no-encryption-block",
			file: rootBackendFile[dirKey], line: 1, registrable: false,
			what: "this Terraform root configures a backend but declares no `terraform { encryption }` " +
				"block, so its state and plans are written in PLAINTEXT — including kubeconfig_raw " +
				"and any provider-computed password. Copy encryption.tf from a sibling root",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out, examined, nil
}

// scanResourceLevers reports every resource whose at-rest argument is absent or
// not "enabled". Brace-counted from the resource head, which is exact for HCL and
// keeps a later resource's argument from vouching for an earlier one — the failure
// a whole-file regex would have.
func scanResourceLevers(body, rel string) []atRestFinding {
	var out []atRestFinding
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		m := reResourceHead.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		lever, watched := atRestResourceLevers[m[1]]
		if !watched {
			continue
		}
		depth, ok := 1, false
		var j int
		for j = i + 1; j < len(lines) && depth > 0; j++ {
			if lever.ok.MatchString(lines[j]) {
				ok = true
			}
			depth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
		}
		if !ok {
			out = append(out, atRestFinding{
				key:  fmt.Sprintf("%s:%s.%s", rel, m[1], m[2]),
				file: rel, line: i + 1, registrable: false,
				what: fmt.Sprintf("%s.%s does not set %s = \"enabled\", so its disks are NOT encrypted "+
					"at rest — every image layer, emptyDir and kubelet-projected Secret on it lands in "+
					"the clear. The argument is ForceNew: it is set at create or never",
					m[1], m[2], lever.arg),
			})
		}
		i = j - 1
	}
	return out
}

// firstMatchLine returns the 1-indexed line of the first match, if any.
func firstMatchLine(body string, re *regexp.Regexp) (int, bool) {
	for i, l := range strings.Split(body, "\n") {
		if re.MatchString(l) {
			return i + 1, true
		}
	}
	return 0, false
}

// relFromRoot renders a scanned path the way the registry keys it: repo-relative
// with forward slashes, so a key reads as something a reviewer can go open.
func relFromRoot(root, path string) string {
	if r, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(path)
}
