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
// rest and nothing in HCL decides whether it is encrypted — the ENDPOINT does
// (see the bucket entries in atRestAllowed). Those resources were invisible here,
// which read as approval rather than as an open question, so they are reported as
// always-registrable findings. See atRestNoLeverResources.
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
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"

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

	// ── Object Storage buckets: encrypted by the ENDPOINT, not by anything here ─
	//
	// Linode applies SSE-S3 (Akamai-managed keys, AES-256) by DEFAULT on E2 and E3
	// endpoints, rolled out 6 Oct – 16 Nov 2026. Nothing a writer sends turns it on
	// and no bucket argument controls it, so there is still no lever for this guard
	// to check — which is why these stay registered rather than leaving the list.
	//
	// What decides the posture is the generation of the endpoint the bucket was
	// created on (`cluster.objectStorage.cluster`), and the hostname does not encode
	// it: us-ord-1 is E1, us-ord-10 is E3. E0/E1 endpoints are NOT covered and their
	// buckets stay plaintext. Objects written before enablement also stay plaintext
	// until rewritten in place (copy-object).
	//
	// So a static guard cannot settle it, and the exits below name the RUNTIME gate
	// that can: `llz ci assert-obj-encryption` reads x-amz-server-side-encryption on
	// real objects and fails on a fresh plaintext one.
	//
	// HISTORY, because the reason these entries read as they did is worth not
	// re-deriving: probed 2026-07-31 on us-ord-10, SSE-S3 then answered 400 and
	// PutBucketEncryption 501, leaving SSE-C as the only mode — which neither Loki
	// nor Harbor can emit. That is what the retired SSE-C gateway existed for; see
	// docs/designs/obj-sse-c-gateway.md.
	//
	// Registered per-bucket rather than once for the module: the two consumers
	// hold materially different data, so a shared entry would let Harbor's story
	// vouch for Loki's.
	"terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.harbor_registry": {
		reason: "every container image layer the platform runs, including anything pushed by tenants. " +
			"Encrypted at rest only if the deployment's object-storage endpoint is E2/E3 — the bucket " +
			"declaration has no say",
		exit: "Linode exposes a bucket-level encryption argument (so this guard can check it), OR the " +
			"posture is accepted as runtime-verified: on an E2/E3 endpoint `llz ci assert-obj-encryption` " +
			"proves a blob pushed by Harbor lands encrypted. Neither is true of an E0/E1 endpoint, whose " +
			"only exit is moving the buckets to E2/E3",
	},
	"terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.loki_chunks": {
		reason: "every log line in the deployment, which is the sharpest of the four: it includes the " +
			"OpenBao audit stream shipped by the promtail sidecar — request paths, operations and " +
			"auth.display_name — so this bucket holds a readable record of who asked OpenBao for what " +
			"wherever the endpoint is E0/E1",
		exit: "as harbor_registry — `llz ci assert-obj-encryption` samples this bucket too. NOTE Loki " +
			"cannot move to block storage instead (the filesystem store cannot serve a clustered " +
			"deployment), so on an E0/E1 endpoint the bucket move is the only route to encryption",
	},
	"terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.loki_ruler": {
		reason: "Loki ruler state — alerting and recording rule definitions. No credentials and no log " +
			"bodies, so the lowest-value of the four; registered separately rather than folded into " +
			"loki_chunks so that closing one cannot silently vouch for the other",
		exit: "same as loki_chunks, and it moves with that bucket: one Loki writes all three, and " +
			"splitting them across two postures gains nothing",
	},
	"terraform-modules/llz-object-storage/main.tf:linode_object_storage_bucket.loki_admin": {
		reason: "Loki admin/tenant metadata. Per ADR 0010 the collector tenant is `admins` and Loki " +
			"runs auth_enabled: true, so this describes the tenancy boundary rather than carrying log " +
			"bodies — but it maps the deployment for anyone who reads it",
		exit: "same as loki_chunks, and it moves with that bucket for the same reason — " +
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
		"encryption argument — the endpoint's generation decides it",
}

type atRestFinding struct {
	key, file, what string
	line            int
	// registrable is false for findings that must be FIXED rather than accepted:
	// a missing `disk_encryption` has no defensible reason, only an unmade
	// decision, so the registry deliberately offers it no escape.
	registrable bool
}

func collectAtRestFindings(repo capability.Repo, dirs []string) ([]atRestFinding, int, error) {
	var out []atRestFinding
	examined := 0
	// Directories that configure a backend, and whether they declare encryption.
	rootHasEncryption := map[string]bool{}
	rootBackendFile := map[string]string{}

	for _, dir := range dirs {
		err := repo.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a missing tree is requireCorpus's problem
			}
			if d.IsDir() {
				if b := d.Name(); b == ".terraform" || b == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".tf" {
				return nil
			}
			b, err := repo.ReadFile(path)
			if err != nil {
				return err
			}
			examined++
			// Already repo-relative: the reader is fenced to the root and hands
			// back paths expressed under it.
			rel := filepath.ToSlash(path)
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
	// decommented for MATCHING, noise for COUNTING. See stripHCL: matching on the
	// raw line let a commented-out resource be scanned as live and a commented-out
	// lever vouch for an unencrypted one.
	decommented, noise := stripHCL(lines)
	for i := 0; i < len(lines); i++ {
		m := reResourceHead.FindStringSubmatch(decommented[i])
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
			if lever.ok.MatchString(decommented[j]) {
				ok = true
			}
			// Braces counted on the line with comments and quoted strings REMOVED;
			// the lever is matched on the line with only COMMENTS removed, because
			// stripping quotes would delete the `"enabled"` it looks for and
			// matching the raw line would let a commented-out one vouch.
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

// relFromRoot is GONE, and its absence is the point: it rendered a scanned path
// repo-relative with forward slashes so a registry key read as something a
// reviewer could open. The read-repo reader is fenced to the root and hands back
// paths already in that shape, so the derivation — and the two failure fallbacks
// under it, which silently keyed a finding on an absolute path — has no caller.

// stripHCL returns two parallel views of the file, both index-aligned with the
// input:
//
//	decommented — comments removed, QUOTED STRINGS KEPT. What every pattern match
//	              runs against, so code that only exists inside a comment cannot
//	              vouch for a resource or be scanned as if it were one.
//	noise       — comments AND quoted strings removed. Brace counting only, so a
//	              `{` inside `label = "a{b"` does not open a block.
//
// MATCHING ON THE RAW LINE WAS THREE FALSE VERDICTS, NOT A SHORTCUT. The lever
// regex, the resource head and the depth walk all ran against the raw text:
//
//   - `encryption = "enabled"` inside a `/* … */` block vouched for an
//     unencrypted resource — a FALSE NEGATIVE on a security gate;
//   - a commented-out `resource "linode_volume" "dead"` was scanned as live, its
//     body lines blank in noise so the depth never closed, and `i = j - 1` then
//     skipped every REAL resource below it;
//   - and the reverse, a finding raised against dead code.
//
// It is also its own scanner rather than shquote.StripSpans, and that is the
// point of the rewrite. StripSpans is a SHELL quoter: it treats a lone `'` as a
// span running to end of line. HCL has no single-quoted strings, so
// `/* don't do this */` lost its `*/`, the block-comment state latched, and the
// rest of the file went unscanned — byte for byte the failure this file's own
// header is about, one layer up from where it was fixed.
//
// HEREDOCS ARE NOT TRACKED. A `<<EOT` body containing an unbalanced brace or a
// `*/` will still confuse the walk; no Terraform in this repo has one inside a
// watched resource, and inventing a parser for it here would be speculative.
func stripHCL(lines []string) (decommented, noise []string) {
	decommented = make([]string, len(lines))
	noise = make([]string, len(lines))
	inBlock := false
	for i, l := range lines {
		var code, bare strings.Builder
		for j := 0; j < len(l); j++ {
			if inBlock {
				if k := strings.Index(l[j:], "*/"); k >= 0 {
					j += k + 1 // +1 more from the loop post
					inBlock = false
					continue
				}
				break // block runs past this line
			}
			switch {
			case l[j] == '"':
				// A double-quoted string is opaque to comments and braces alike:
				// it survives into decommented and is dropped from noise.
				code.WriteByte('"')
				for j++; j < len(l) && l[j] != '"'; j++ {
					if l[j] == '\\' && j+1 < len(l) {
						code.WriteByte(l[j])
						j++
					}
					code.WriteByte(l[j])
				}
				if j < len(l) {
					code.WriteByte('"')
				}
			case l[j] == '#', strings.HasPrefix(l[j:], "//"):
				j = len(l) // rest of the line is a comment
			case strings.HasPrefix(l[j:], "/*"):
				if k := strings.Index(l[j+2:], "*/"); k >= 0 {
					j += 2 + k + 1
					continue
				}
				inBlock = true
				j = len(l)
			default:
				code.WriteByte(l[j])
				bare.WriteByte(l[j])
			}
		}
		decommented[i], noise[i] = code.String(), bare.String()
	}
	return decommented, noise
}

// stripLineComments removes comments from ONE already-string-stripped line,
// reporting whether an unterminated /* was left open.
//
// LEFT TO RIGHT, TAKING WHICHEVER OPENER COMES FIRST — and doing it in a fixed
// order instead is a false negative on a security gate. stripHCLNoise used to
// search for `/*` BEFORE cutting `#` and `//`, so an ordinary HCL line comment
// containing a glob —
//
//	# see modules/*/main.tf
//
// — matched `/*` inside `modules/*`, found no closing `*/`, and put the scanner
// into block-comment mode FOR THE REST OF THE FILE. Brace counting stopped, the
// depth walk ran to EOF, and every resource below that line went silently
// unscanned. Probe-verified on the shipped fixture: 2 findings without the
// comment, 1 with it — the resource below the glob line is the one that goes
// unexamined.
//
// Reversing the order does not fix it, which is worth stating because it is the
// obvious move: cutting `#` first breaks `x = 1 /* note # */` by truncating
// mid-block-comment and opening the same run-past one case over. Only "whichever
// opener appears first wins" is correct for both.
func stripLineComments(code string) (string, bool) {
	var kept strings.Builder
	for {
		h, sl, bl := strings.Index(code, "#"), strings.Index(code, "//"), strings.Index(code, "/*")
		first, kind := -1, ""
		for _, c := range []struct {
			at   int
			kind string
		}{{h, "line"}, {sl, "line"}, {bl, "block"}} {
			if c.at >= 0 && (first < 0 || c.at < first) {
				first, kind = c.at, c.kind
			}
		}
		if first < 0 {
			kept.WriteString(code)
			return kept.String(), false
		}
		kept.WriteString(code[:first])
		if kind == "line" {
			return kept.String(), false
		}
		rest := code[first+2:]
		e := strings.Index(rest, "*/")
		if e < 0 {
			return kept.String(), true // block comment runs past this line
		}
		code = rest[e+2:]
	}
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
// here that is requireCorpus in internal/cli, which owns corpus LOCATION (the
// template-vs-instance layout resolution eight guards share) while this package
// owns what a Terraform file has to say.
type Findings struct {
	Residuals []atRestFinding
	Examined  int
}

// Scan walks dirs for Terraform and reports every resource that is not encrypted
// at rest, plus every registry entry that no longer matches anything.
func Scan(repo capability.Repo, dirs []string) (Findings, error) {
	f, examined, err := collectAtRestFindings(repo, dirs)
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
func ScanDirs(repo capability.Repo) []string {
	return []string{
		guardkit.RepoPath(repo, filepath.Join("tools", "internal", "shared", "tfroots", "roots")),
		guardkit.RepoPath(repo, "terraform-modules"),
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
	repo := capability.RepoAt(atRestBinding(), root)
	dirs := ScanDirs(repo)
	f, err := Scan(repo, dirs)
	if err != nil {
		return err
	}
	if err := guardkit.RequireCorpus("at-rest-guard", f.Examined, dirs); err != nil {
		return err
	}
	return Report(out, f)
}
