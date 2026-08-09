package atrest

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
//                              holds the bucket key (ADR 0007 (state encryption)).
//   disk_encryption            the node pool's boot and data disks: every image
//                              layer, every emptyDir, and the kubelet's on-disk
//                              copy of every Secret projected into a pod.
//   encryption (linode_volume) a directly-declared Volume. The CSI-provisioned
//                              ones are covered at runtime by
//                              `assert-volume-encryption`, which reads the Linode
//                              API; nothing covered a Volume declared in HCL.
//
// A FOURTH CLASS has no lever at all: linode_object_storage_bucket holds data at
// rest and Linode exposes no way to encrypt it (measured — see the bucket entries
// in atRestAllowed for the probe and its numbers). Those resources were invisible
// here, which read as approval rather than as an open question, so they are now
// reported as always-registrable findings. See atRestNoLeverResources.
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
// SCANNER LIMIT. Block scope is found by counting braces on lines with comments
// and quoted strings removed (stripHCLNoise) — a brace in a comment used to run
// the counter past the end of its resource and silently hide every later one.
// HEREDOCS are NOT stripped: a `<<-EOT` body containing an unbalanced brace would
// reintroduce that. No heredoc in this tree sits inside a `resource` block (they
// are all `description` on variables), so it is a stated limit rather than a live
// hole — and it fails toward a false NEGATIVE, which is why it is written down.
//
// THE ONE RESIDUE IS REGISTERED, NOT IGNORED. ADR 0007 (state encryption) shipped its migration in
// two phases and only phase 1 has happened: all four roots still carry
// `fallback { method = method.unencrypted.migrate }`, which is what lets OpenTofu
// READ pre-encryption state. It also means an unencrypted state file is still
// accepted rather than refused. That is a deliberate, documented position — and
// it was a comment repeated in four files with no owner and no exit condition,
// which is how a two-phase migration becomes a one-phase one. Registering it puts
// the debt in one place with the test for retiring it.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/shquote"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/guardkit"
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
	// ── ADR 0007 (state encryption) phase 1: the unencrypted fallback ───────────────────────────
	//
	// One entry per root rather than one shared entry, deliberately: retiring this
	// is per-root work (each root's state has to have been migrated before its
	// fallback can go), and a single entry would let the first root that migrated
	// vouch for the three that had not.
	"tools/internal/shared/tfroots/roots/cluster/encryption.tf:unencrypted-fallback": {
		reason: "ADR 0007 (state encryption) PHASE 1. `enforced` and an UNENCRYPTED fallback are mutually exclusive, " +
			"so an existing instance cannot go straight to enforced — the fallback is what reads " +
			"pre-encryption state and rewrites it encrypted. While it is here, a state file that " +
			"was written in plaintext is ACCEPTED rather than refused, and this root's state holds " +
			"kubeconfig_raw: cluster-admin on the deployment, readable by anyone with the bucket key",
		exit: "every deployment's cluster state has been migrated (a no-op apply does it); then " +
			"delete the fallback block and set `enforced = true` in its place. terraform-init " +
			"already declares method.unencrypted.migrate on its side, so phase 2 needs no change " +
			"to the action — only to this file",
	},
	"tools/internal/shared/tfroots/roots/databases/encryption.tf:unencrypted-fallback": {
		reason: "ADR 0007 (state encryption) PHASE 1, as cluster/. This root's state is the sharpest of the four: " +
			"`root_password` is a provider-COMPUTED attribute on every Managed Postgres cluster, " +
			"so there is no way to keep it out of state at all — encryption is the only control",
		exit: "as cluster/ — migrate this root's state in every deployment, then swap the fallback " +
			"for `enforced = true`",
	},
	"tools/internal/shared/tfroots/roots/object-storage/encryption.tf:unencrypted-fallback": {
		reason: "ADR 0007 (state encryption) PHASE 1, as cluster/. This root's state carries OBJ key material — " +
			"including, historically, the keys that reach the Loki and Harbor buckets",
		exit: "as cluster/ — migrate this root's state in every deployment, then swap the fallback " +
			"for `enforced = true`",
	},
	"tools/internal/shared/tfroots/roots/vpc/encryption.tf:unencrypted-fallback": {
		reason: "ADR 0007 (state encryption) PHASE 1, as cluster/. The lowest-value state of the four (network " +
			"topology, no credential), and it moves with the others rather than separately: a " +
			"half-migrated instance split across two postures is worse to reason about than either " +
			"posture applied uniformly",
		exit: "as cluster/ — migrate this root's state in every deployment, then swap the fallback " +
			"for `enforced = true`",
	},

	// ── Object Storage buckets: no SSE mode Linode implements is reachable ───
	//
	// MEASURED, not inferred — this is the fact ADR 0007 (state encryption) recorded as "unverified"
	// and it is the whole reason these four entries read the way they do. Probed
	// 2026-07-31 against a scratch bucket on us-ord-10 (E3) with a temporary
	// scoped key, all of it deleted afterwards:
	//
	//   plain PUT then HEAD      200, NO x-amz-server-side-encryption header —
	//                            nothing is applied by default
	//   SSE-S3 (AES256 header)   400 InvalidArgument
	//   SSE-C (customer key)     200; HEAD without the key 400 — it genuinely works
	//   PutBucketEncryption      501 NotImplemented
	//   GetBucketEncryption      501 NotImplemented
	//
	// Those numbers are from us-ord-10 (E3). SSE-C is also supported on E1, which
	// matters because not every deployment is on a gen-2 endpoint: the ohttp and lab
	// buckets sit on us-ord-1 (E1). So the gateway covers both generations rather
	// than only the newest.
	//
	// So the two obvious moves are both dead, and one of them is dead in the
	// DANGEROUS direction. Harbor's registry (`encrypt: true`) and Loki
	// (`sse.type: SSE-S3`) can each request SSE-S3 in one line of values — and on
	// Linode that returns 400 on every blob push and every chunk flush. It does
	// not degrade to plaintext; it breaks the writer. Nobody should discover that
	// from a production rollout, which is why the numbers are written here rather
	// than summarised as "not supported".
	//
	// SSE-C is the one mode Linode implements and NEITHER writer can emit it:
	// Loki's SSEConfig accepts only SSE-KMS and SSE-S3 and hard-errors otherwise,
	// and distribution's S3 driver exposes only `encrypt` and `keyid`. Reaching it
	// means forking both.
	//
	// AND IT WOULD BUY LESS THAN IT LOOKS. SSE-C keys travel on every GET, so the
	// key would have to sit in the same OpenBao path and the same mounted Secret
	// as the S3 credential the app already holds. That defends against obtaining
	// bucket CONTENTS without the app's config — Linode-side disk access, a stray
	// listing — but not against the access-key compromise ADR 0007 (state encryption) names as the
	// real blast radius. Worth stating so a future reader weighs the fork against
	// what it actually closes.
	//
	// Registered per-bucket rather than once for the module: the two consumers
	// hold materially different data and would be fixed by different upstreams, so
	// a shared entry would let Harbor's story vouch for Loki's.
	"terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.harbor_registry": {
		reason: "every container image layer the platform runs, including anything pushed by tenants. " +
			"Readable by whoever holds a key scoped to this bucket. Closest thing to a fix that exists " +
			"today is distribution gaining SSE-C support, which would still need Linode to keep " +
			"honouring it",
		exit: "the objProxy component is ENABLED for this deployment and `llz ci assert-obj-encryption` " +
			"reports green against this bucket — which requires the CoreDNS rewrite live, the Kyverno " +
			"harbor-obj-proxy-ca mutation on the running registry pods, and a real object answering 400 " +
			"to a keyless HEAD. Until all three hold, the bucket is plaintext no matter what is deployed. " +
			"(The old exits — Linode shipping SSE-S3, or distribution gaining SSE-C — remain valid but are " +
			"no longer what this is waiting on.)",
	},
	"terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.loki_chunks": {
		reason: "every log line in the deployment, which is the sharpest of the four: it includes the " +
			"OpenBao audit stream shipped by the promtail sidecar — request paths, operations and " +
			"auth.display_name — so this bucket holds a readable record of who asked OpenBao for what",
		exit: "as harbor_registry — the objProxy gateway covers this bucket too, and Loki needs NO CA " +
			"work to use it (its rendered config carries insecure_skip_verify: true on the S3 client, " +
			"measured on the live cluster). NOTE this bucket has no alternative: Loki cannot move to " +
			"block storage, because the filesystem store cannot serve a clustered deployment and this " +
			"one runs three ingesters behind separate queriers — the proxy is the only route to " +
			"encryption here",
	},
	"terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.loki_ruler": {
		reason: "Loki ruler state — alerting and recording rule definitions. No credentials and no log " +
			"bodies, so the lowest-value of the four; registered separately rather than folded into " +
			"loki_chunks so that closing one cannot silently vouch for the other",
		exit: "same two triggers as loki_chunks — a 200 on the SSE-S3 probe, or an SSE-C type in Loki's " +
			"SSEConfig. This bucket moves with loki_chunks rather than separately: they are written by " +
			"the same Loki and split across two postures for no gain",
	},
	"terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.loki_admin": {
		reason: "Loki admin/tenant metadata. Per ADR 0010 the collector tenant is `admins` and Loki " +
			"runs auth_enabled: true, so this describes the tenancy boundary rather than carrying log " +
			"bodies — but it maps the deployment for anyone who reads it",
		exit: "same two triggers as loki_chunks, and it moves with that bucket for the same reason — " +
			"one Loki writes all three, so retiring them independently would leave a split posture " +
			"nobody could describe in one sentence",
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
	// The posture block ADR 0007 (state encryption) puts in code, deliberately separate from the key
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

// atRestNoLeverResources are resource types that hold data at rest and have NO
// argument to encrypt it — so unlike the levered types above, there is nothing
// to set and the finding is always REGISTRABLE.
//
// They exist as their own class because the guard was blind to them in a way
// that read as approval: the levered map is a list of things to check, and a
// resource absent from it is simply never looked at. Four buckets holding every
// container image and every log line in the deployment sat outside it, and the
// guard printed green over them — which is exactly the "cannot tell which lines
// are load-bearing" failure the registry exists to prevent, one level up.
//
// Being registrable is the point: the gap cannot be closed, so the useful
// artifact is a reviewed statement of what is exposed plus a condition that
// retires it, not a check nobody can satisfy.
var atRestNoLeverResources = map[string]string{
	"linode_object_storage_bucket": "Linode Object Storage exposes no bucket-level " +
		"encryption argument, and the provider surfaces none",
}

type atRestFinding struct {
	key, file, what string
	line            int
	// registrable is false for findings that must be FIXED rather than accepted:
	// a missing `disk_encryption` has no defensible reason, only an unmade
	// decision, so the registry deliberately offers it no escape.
	registrable bool
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
	noise := stripHCLNoise(lines)
	for i := 0; i < len(lines); i++ {
		m := reResourceHead.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		// No-lever types first: there is no argument to look for, so the finding
		// is emitted from the head line alone and the body is not scanned.
		if why, unlevered := atRestNoLeverResources[m[1]]; unlevered {
			out = append(out, atRestFinding{
				key:  fmt.Sprintf("%s:%s.%s", rel, m[1], m[2]),
				file: rel, line: i + 1, registrable: true,
				what: fmt.Sprintf("%s.%s stores data at rest with no encryption argument available (%s), "+
					"so its contents are protected only by whatever the provider does underneath",
					m[1], m[2], why),
			})
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
			// Braces counted on the line with comments and quoted strings REMOVED,
			// while the lever is matched on the raw line (stripping quotes would
			// delete the `"enabled"` it looks for).
			//
			// This is not fussiness. Terraform in this repo is heavily commented
			// with backticked code fragments, and one comment containing an
			// unbalanced `{` runs the depth counter past the end of its resource,
			// swallowing every LATER resource in the file — which the outer loop
			// then skips via `i = j - 1`. The failure is silent and it is a
			// FALSE NEGATIVE on a security gate: the guard reports green having
			// never looked at the resource.
			depth += braceDelta(noise[j])
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

// stripHCLNoise returns a parallel slice of lines with block comments, line
// comments and quoted strings blanked out, for brace counting only. Indices line
// up with the input so a caller can match on the raw line and count on this one.
//
// Quoted spans go first, so a `#` or `{` inside a string is already gone by the
// time the comment cut runs — `label = "a#b{c"` must not read as a comment or as
// an opening brace.
func stripHCLNoise(lines []string) []string {
	out := make([]string, len(lines))
	inBlock := false
	for i, l := range lines {
		// /* … */ can span lines, and an unclosed one has the same silent
		// run-past effect as an unbalanced brace, so it is tracked rather than
		// ignored.
		if inBlock {
			if k := strings.Index(l, "*/"); k >= 0 {
				l, inBlock = l[k+2:], false
			} else {
				out[i] = ""
				continue
			}
		}
		code, _ := shquote.StripSpans(l)
		if k := strings.Index(code, "/*"); k >= 0 {
			if e := strings.Index(code[k:], "*/"); e >= 0 {
				code = code[:k] + code[k+e+2:]
			} else {
				code, inBlock = code[:k], true
			}
		}
		if k := strings.Index(code, "#"); k >= 0 {
			code = code[:k]
		}
		if k := strings.Index(code, "//"); k >= 0 {
			code = code[:k]
		}
		out[i] = code
	}
	return out
}

// braceDelta is the net HCL block-depth change of one already-denoised line.
func braceDelta(code string) int {
	return strings.Count(code, "{") - strings.Count(code, "}")
}

// Findings is one scan's result: the residuals found, and how many files were
// actually examined.
//
// EXAMINED IS RETURNED, NOT JUST LOGGED, because "found nothing" and "looked at
// nothing" print the same green. The caller decides what an empty corpus means —
// here that is requireCorpus in package main, which owns corpus LOCATION (the
// template-vs-instance layout resolution eight guards share) while this package
// owns what a Terraform file has to say.
type Findings struct {
	Residuals []atRestFinding
	Examined  int
}

// Scan walks dirs for Terraform and reports every resource that is not encrypted
// at rest, plus every registry entry that no longer matches anything.
func Scan(root string, dirs []string) (Findings, error) {
	f, examined, err := collectAtRestFindings(root, dirs)
	return Findings{Residuals: f, Examined: examined}, err
}

// Report prints the verdict and returns an error when the gate fails. The writer
// is a parameter for the reason the budget engine learned the expensive way: a
// gate's ::error:: annotations are its product, and GitHub silently stops
// rendering one that loses its line-start position while the build still exits 1.
func Report(out io.Writer, f Findings) error {
	seen := map[string]bool{}
	failed := false
	for _, fd := range f.Residuals {
		rule, ok := atRestAllowed[fd.key]
		if fd.registrable {
			seen[fd.key] = true
		}
		if ok && fd.registrable {
			fmt.Fprintf(out, "  ok: %s:%d %s — %s (exit: %s)\n", fd.file, fd.line, fd.what, rule.reason, rule.exit)
			continue
		}
		failed = true
		if !fd.registrable {
			fmt.Fprintf(out, "::error file=%s,line=%d::%s. This one has no registry escape: the argument is "+
				"ForceNew and there is no reason to declare a resource unencrypted, only an unmade "+
				"decision. Set it.\n", fd.file, fd.line, fd.what)
			continue
		}
		fmt.Fprintf(out, "::error file=%s,line=%d::%s. Either close it, or register %q in atRestAllowed "+
			"(tools/internal/extensions/lifecycle/atrest/atrest.go) with a reason naming WHAT is exposed and an exit condition "+
			"that retires the entry. See docs/adr/0007-terraform-state-encryption.md.\n",
			fd.file, fd.line, fd.what, fd.key)
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
		fmt.Fprintf(out, "::error::atRestAllowed entry %q matches nothing in the tree. The gap was closed "+
			"or moved — delete the entry. A registry that keeps dead entries stops being reviewable, "+
			"because a reader cannot tell which lines are load-bearing.\n", k)
	}

	if failed {
		return fmt.Errorf("at-rest-guard: unencrypted resource(s) and/or stale registry entries")
	}
	fmt.Fprintf(out, "at-rest-guard: %d file(s) examined, %d registered at-rest residual(s).\n", f.Examined, len(f.Residuals))
	return nil
}

// ScanDirs are the trees this repo AUTHORS Terraform in: the embedded root
// templates every instance is scaffolded from, and the published modules they
// call. Resolved through guardkit.RepoPath for the same reason the sibling guards
// do — a raw join silently resolves to a non-existent path in the instance layout,
// and a missing tree is tolerated by the walk, so the guard would scan less than
// it appears to.
func ScanDirs(root string) []string {
	return []string{
		guardkit.RepoPath(root, filepath.Join("tools", "internal", "shared", "tfroots", "roots")),
		guardkit.RepoPath(root, "terraform-modules"),
	}
}

// Run is the whole gate: locate, scan, refuse an empty corpus, report.
//
// It exists so the command is a flag set and nothing else, and so the package's
// own tests can exercise the gate end to end. Corpus location moved in here with
// ScanDirs once guardkit existed — before that it had to stay in package main,
// which would have split these tests across a package boundary for no reason but
// where a helper happened to live.
func Run(out io.Writer, root string) error {
	dirs := ScanDirs(root)
	f, err := Scan(root, dirs)
	if err != nil {
		return err
	}
	if err := guardkit.RequireCorpus("at-rest-guard", f.Examined, dirs); err != nil {
		return err
	}
	return Report(out, f)
}
