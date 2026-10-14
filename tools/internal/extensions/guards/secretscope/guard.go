package secretscope

// guard.go implements `llz ci workflow-secret-scope` — an environment-scoped
// secret resolves only inside a job that declares an `environment:`, and an
// infra-<env> environment is reachable only from `main`.
//
// WHY: BOTH HALVES FAIL SILENTLY, AND THEY FAIL DIFFERENTLY.
//
// GitHub resolves `secrets.X` against the REPO scope plus the job's environment,
// if it declares one. An env-scoped secret read from a job with no `environment:`
// is not an error — it is the empty string. Nothing in the log says a secret was
// asked for and not found; what the operator sees is whatever the tool does with
// an empty credential, several layers from the cause.
//
// llz-terraform.yml's `plan-cluster-pr` shipped exactly that. It read
// TF_STATE_ACCESS_KEY, TF_STATE_SECRET_KEY and LINODE_API_TOKEN — all three marked
// EnvScope in envreq's requirement table, because the state bucket and the Linode
// credentials are per-deployment — from a job with no `environment:`. Every plan
// on every adopter's PR died at `tofu init` against an S3 backend it had no
// credentials for. It survived because the ONE pipeline that opens a PR against a
// real instance, release-e2e's PR-gate probe, opens it as a DRAFT, and the job
// skipped drafts for an unrelated reason.
//
// AND THE OBVIOUS REPAIR IS THE SECOND HALF. Adding `environment: infra-<env>`
// makes the secrets resolve on paper and cannot work: `llz` locks every infra-<env>
// environment to a deployment-branch-policy of `main` only (branchpolicy.Lock),
// and that lock is the boundary stopping someone from pushing a branch,
// dispatching against infra-prod and having GitHub inject the OpenBao unseal keys
// into a job their branch controls. A pull_request run's ref is refs/pull/N/merge,
// which no branch policy matches — so the environment form fails at job START.
// Unlocking the environment for PR branches trades a plan preview for the exact
// hole the lock exists to close. There is no third position: a pull-request job
// simply cannot hold these credentials, and the guard says so rather than letting
// the next author rediscover it one wrong fix at a time.
//
// THE EXPECTED SET IS NOT DERIVED FROM THE THING UNDER TEST. Which secrets are
// env-scoped comes from envreq.E2ERequirements — the same table `llz doctor`,
// `llz tokens` and `require-repo-config` read — not from the workflows. A workflow
// that stops mentioning a secret cannot shrink the set it is checked against.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envreq"
)

// scanRoots are the workflow trees checked. instance-template's is the one that
// matters most — those files are DELIVERED, so a secret that resolves to nothing
// there is every adopter's — and it is where the one real instance of this bug
// lived. The first-party tree is scanned too because this repo's own workflows
// consume the same table's secrets on the e2e path.
var scanRoots = []string{".github/workflows", "instance-template/.github/workflows"}

// localCallPrefix marks a reusable call this guard can resolve. A remote
// `uses: org/repo/.github/workflows/x.yml@ref` cannot be read from this tree, so
// reachability stops there rather than being guessed at.
const localCallPrefix = "./.github/workflows/"

// prEvents are the triggers that put a run on a pull-request ref. Both, because
// pull_request_target differs from pull_request in which ref the CODE comes from,
// not in the ref the DEPLOYMENT is evaluated against.
var prEvents = map[string]bool{"pull_request": true, "pull_request_target": true}

// secretRefRe matches every spelling of a secret reference GitHub accepts:
// `secrets.NAME` and the bracket forms `secrets['NAME']` / secrets["NAME"].
// Missing the bracket form would let a rename through the one syntax nobody
// grep's for.
var secretRefRe = regexp.MustCompile(`secrets\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_]*)|\[\s*['"]([^'"]+)['"]\s*\])`)

// eventNameRe matches `github.event_name == 'x'`, the only way a job in these
// trees narrows itself to one trigger.
var eventNameRe = regexp.MustCompile(`github\.event_name\s*==\s*'([a-z_]+)'`)

// workflow is the sliver of an Actions file this guard reads. Jobs stay as raw
// nodes so a job can be both INSPECTED (environment, if, uses) and re-rendered to
// text for the secret scan — the scan has to see `with:`, `env:`, `run:` and the
// `secrets:` mapping of a reusable call alike, and enumerating those keys is how
// the next place a secret can be written gets missed.
type workflow struct {
	On yaml.Node `yaml:"on"`
	// Env is the WORKFLOW-LEVEL env: block, and leaving it out was a hole big
	// enough to drive the whole finding through. A workflow that hoists
	// `AWS_ACCESS_KEY_ID: ${{ secrets.TF_STATE_ACCESS_KEY }}` up here — which is
	// ordinary style, and which the delivered pipeline already does for other
	// values — puts the reference outside every job body, so scanning jobs alone
	// saw nothing and the gate passed. The env block applies to EVERY job, so its
	// secrets are attributed to every job.
	Env  yaml.Node            `yaml:"env"`
	Jobs map[string]yaml.Node `yaml:"jobs"`
}

// jobHead is the part of a job this guard decides on.
type jobHead struct {
	Environment yaml.Node `yaml:"environment"`
	If          string    `yaml:"if"`
	Uses        string    `yaml:"uses"`
}

// Finding is one job holding an env-scoped secret it cannot resolve.
type Finding struct {
	File    string
	Job     string
	Secrets []string
	Kind    Kind
	// Env is the environment the job declares, when it declares one. Printed
	// because the PR-reachable finding is about a job that looks correct.
	Env string
	// Via is the pull-request-triggered workflow this job was reached from, empty
	// when the job's own workflow carries the trigger. The reader needs it: a job
	// in a reusable body has nothing in its own file saying a PR can start it.
	Via string
}

// Kind separates the two ways the same secret goes missing, because they have
// nothing in common as remedies.
type Kind int

const (
	// NoEnvironment — the job reads an env-scoped secret and declares no
	// environment, so the value arrives empty. Fix: declare the environment.
	NoEnvironment Kind = iota
	// PRReachable — the job reads an env-scoped secret on a pull-request path,
	// where the environment holding it is branch-locked to main and cannot be
	// entered at all. Fix: do not read it there.
	PRReachable
)

// EnvScopedSecrets is the expected set, taken from the requirement table rather
// than from the workflows. admin=true so the template repo's own e2e-harness
// entries are included; those are repo-level today, and reading the table with
// the flag set means they are covered the day one is not.
func EnvScopedSecrets() map[string]bool {
	out := map[string]bool{}
	for _, r := range envreq.E2ERequirements(true) {
		if r.Secret && r.EnvScope {
			out[r.Name] = true
		}
	}
	return out
}

// triggersOnPR reports whether a workflow's `on:` names a pull-request event. It
// handles all three shapes GitHub accepts — a bare string, a sequence, and a
// mapping — because a workflow written in the sequence form would otherwise read
// as having no triggers at all and take every job in it out of scope silently.
func triggersOnPR(on yaml.Node) bool {
	switch on.Kind {
	case yaml.ScalarNode:
		return prEvents[on.Value]
	case yaml.SequenceNode:
		for _, c := range on.Content {
			if prEvents[c.Value] {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			if prEvents[on.Content[i].Value] {
				return true
			}
		}
	}
	return false
}

// jobRunsOnPR reports whether a job inside a pull-request-reachable workflow can
// itself run on that path. A job whose `if:` names `github.event_name` equalities
// and lists no pull-request event among them is excluded — that is how every
// dispatch-only job in the delivered pipeline is written, and without honouring it
// this guard would flag `apply-cluster` for holding the credentials it exists to
// hold.
//
// Deliberately conservative in the other direction: a job with NO event gate in a
// PR-reachable workflow counts as PR-reachable. "I could not tell" and "it does
// not run there" are not the same answer, and only one of them is safe to assume
// about a job holding production credentials.
func jobRunsOnPR(cond string) bool {
	events := eventNameRe.FindAllStringSubmatch(cond, -1)
	if len(events) == 0 {
		return true
	}
	for _, m := range events {
		if prEvents[m[1]] {
			return true
		}
	}
	return false
}

// commentLineRe matches a whole-line YAML comment in the re-marshalled node.
var commentLineRe = regexp.MustCompile(`(?m)^[ \t]*#.*$`)

// secretsIn returns the env-scoped secrets a job's YAML references, sorted.
//
// COMMENTS ARE NOT REFERENCES, and yaml.Marshal round-trips them. These workflows
// carry more prose than YAML — the note where the retired plan job used to be
// NAMES the secrets it could not resolve — so scanning the re-marshalled node
// verbatim reports a comment as a live reference and fails the gate on a file
// that is correct. A guard that cries wolf gets deleted, and this one would have
// cried on the very commit that introduced it if that note had sat inside a job.
//
// Whole-line comments only. A trailing `# …` after real YAML cannot introduce a
// secret reference the line does not already make, and blanking to end-of-line
// would need to know whether the `#` is inside a quoted scalar — which is the
// yaml package's job, not a regex's.
func secretsIn(node yaml.Node, scoped map[string]bool) []string {
	raw, err := yaml.Marshal(&node)
	if err != nil {
		return nil
	}
	body := commentLineRe.ReplaceAllString(string(raw), "")
	seen := map[string]bool{}
	for _, m := range secretRefRe.FindAllStringSubmatch(body, -1) {
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if scoped[name] {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// union merges two sorted secret-name lists, de-duplicated and sorted.
func union(a, b []string) []string {
	seen := map[string]bool{}
	for _, v := range append(append([]string{}, a...), b...) {
		seen[v] = true
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// envOf renders a job's `environment:`, which is either a scalar name or a
// mapping with `name:`. Returns "" when the job declares none.
func envOf(n yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Value
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "name" {
				return n.Content[i+1].Value
			}
		}
		// A mapping without `name:` is malformed for Actions, but it IS an
		// environment declaration — reporting it as absent would name the wrong
		// defect.
		return "(unnamed)"
	}
	return ""
}

// calleePath resolves a local reusable call to a path in the same tree the caller
// lives in. `./.github/workflows/x.yml` inside instance-template/ means
// instance-template/.github/workflows/x.yml, not the template's own copy — the
// delivered pipeline is self-contained on purpose, and resolving across the trees
// would make the first-party file answer for the delivered one.
func calleePath(caller, uses string) (string, bool) {
	if !strings.HasPrefix(uses, localCallPrefix) {
		return "", false
	}
	dir := path.Dir(caller) // <tree>/.github/workflows
	tree := path.Dir(path.Dir(dir))
	return path.Join(tree, strings.TrimPrefix(uses, "./")), true
}

// prReachable returns the set of files a pull request can start, following local
// reusable calls from the jobs that can themselves run on that path. Propagating
// through the CALLING JOB's gate rather than through the file is the difference
// between "llz-bootstrap-openbao.yml's jobs hold env secrets on a PR" (they do
// not — their one caller is dispatch-only) and a page of false findings.
func prReachable(files map[string]workflow) map[string]string {
	reach := map[string]string{}
	for p, w := range files {
		if triggersOnPR(w.On) {
			reach[p] = ""
		}
	}
	for changed := true; changed; {
		changed = false
		for p := range reach {
			w := files[p]
			for name, node := range w.Jobs {
				var h jobHead
				if node.Decode(&h) != nil || !jobRunsOnPR(h.If) {
					continue
				}
				c, ok := calleePath(p, h.Uses)
				if !ok {
					continue
				}
				if _, known := files[c]; !known {
					continue
				}
				if _, already := reach[c]; already {
					continue
				}
				// Name the ENTRY POINT, not the immediate caller: what the reader
				// needs is the pull_request trigger, which may be two hops up.
				via := p + " (job " + name + ")"
				if v := reach[p]; v != "" {
					via = v
				}
				reach[c] = via
				changed = true
			}
		}
	}
	return reach
}

// Scan is the whole decision, over an already-parsed corpus. Pure, so the arms
// below are testable without a tree.
func Scan(files map[string]workflow, scoped map[string]bool) []Finding {
	reach := prReachable(files)
	var findings []Finding
	for p, w := range files {
		via, onPR := reach[p]
		// Workflow-level env: applies to every job in the file, so a secret named
		// there is a secret every job reads.
		wide := secretsIn(w.Env, scoped)
		for name, node := range w.Jobs {
			used := union(secretsIn(node, scoped), wide)
			if len(used) == 0 {
				continue
			}
			var h jobHead
			_ = node.Decode(&h)
			env := envOf(h.Environment)
			switch {
			case onPR && jobRunsOnPR(h.If):
				findings = append(findings, Finding{File: p, Job: name, Secrets: used, Kind: PRReachable, Env: env, Via: via})
			case env == "":
				findings = append(findings, Finding{File: p, Job: name, Secrets: used, Kind: NoEnvironment})
			}
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Job < findings[j].Job
	})
	return findings
}

// Run reads the corpus and reports.
func Run(root string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)

	scoped := EnvScopedSecrets()
	if len(scoped) == 0 {
		// The expected set going empty would make every arm below pass over
		// everything. It comes from a table in this binary, so an empty one is a
		// code change, not a corpus problem — and it is the single edit that
		// disarms the guard completely.
		return fmt.Errorf("workflow-secret-scope: envreq's requirement table names no environment-scoped secrets — " +
			"refusing to pass vacuously over every workflow")
	}

	files := map[string]workflow{}
	perRoot := map[string]int{}
	for _, r := range scanRoots {
		err := repo.WalkDir(filepath.FromSlash(r), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				// An absent root is legitimate: this also runs in an instance
				// checkout, which has no instance-template/ of its own.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || !(strings.HasSuffix(d.Name(), ".yml") || strings.HasSuffix(d.Name(), ".yaml")) {
				return nil
			}
			b, rerr := repo.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			var w workflow
			if uerr := yaml.Unmarshal(b, &w); uerr != nil {
				// A workflow this guard cannot parse is one it cannot vouch for.
				return fmt.Errorf("%s: %w", filepath.ToSlash(p), uerr)
			}
			files[filepath.ToSlash(p)] = w
			perRoot[r]++
			return nil
		})
		if err != nil {
			return fmt.Errorf("workflow-secret-scope: %w", err)
		}
	}

	findings := Scan(files, scoped)

	// VACUITY IS ONLY A QUESTION ON THE PASS PATH. Checked before the report, a
	// corpus complaint would pre-empt a real finding and name the wrong problem in
	// the one case this exists for.
	if len(findings) == 0 {
		if len(files) == 0 {
			return fmt.Errorf("workflow-secret-scope: no workflows found under %s — refusing to pass vacuously",
				strings.Join(scanRoots, ", "))
		}
		// PER ROOT, not just in total. One aggregate count cannot tell "this root
		// is legitimately absent" from "this root moved" — and the half that goes
		// missing that way is the DELIVERED one, since the first-party tree always
		// resolves. The result would be a confident OK over the template's own
		// files with every file that reaches an adopter unread.
		for _, r := range scanRoots {
			if perRoot[r] > 0 {
				continue
			}
			if _, err := repo.Stat(filepath.FromSlash(r)); err == nil {
				return fmt.Errorf("workflow-secret-scope: %s exists but holds no workflows — "+
					"a root this guard scans must not be empty", r)
			}
			// THE TREE ROOT, not the root's parent: the parent of a delivered root
			// is itself inside the tree a rename moves.
			tree, _, nested := strings.Cut(r, "/")
			if !nested || tree == ".github" {
				return fmt.Errorf("workflow-secret-scope: %s yielded no workflows — "+
					"the tree moved and this run would have passed over it", r)
			}
			if _, err := repo.Stat(filepath.FromSlash(tree)); err != nil {
				continue // no instance-template/ at all: an instance checkout.
			}
			return fmt.Errorf("workflow-secret-scope: %s yielded no workflows but %s exists — "+
				"the delivered workflow tree moved and this run would have passed over it", r, tree)
		}
		fmt.Fprintf(out, "workflow-secret-scope: OK — %d workflow(s), %d environment-scoped secret(s) checked\n",
			len(files), len(scoped))
		return nil
	}

	for _, f := range findings {
		fmt.Fprintf(errOut, "::error file=%s::job %q reads %s — %s\n",
			f.File, f.Job, strings.Join(f.Secrets, ", "), f.headline())
	}
	fmt.Fprintf(errOut, "\n%s %d job(s) read an environment-scoped secret they cannot resolve:\n", color.Red("✗"), len(findings))
	for _, f := range findings {
		fmt.Fprintf(errOut, "    %s  job %q\n        %s\n        secrets: %s\n",
			f.File, f.Job, f.detail(), strings.Join(f.Secrets, ", "))
	}
	fmt.Fprint(errOut, "\nGitHub resolves an environment-scoped secret only inside a job that declares the\n"+
		"`environment:` holding it; everywhere else it arrives as the EMPTY STRING, with\n"+
		"nothing in the log saying so. And `llz` locks every infra-<env> environment to\n"+
		"ref=main (branchpolicy.Lock) — the boundary that stops a pushed branch from\n"+
		"having the OpenBao unseal keys injected into a job it controls — so a\n"+
		"pull_request run, whose ref is refs/pull/N/merge, cannot enter one at all.\n\n"+
		"Fix: declare `environment: infra-<deployment>` on a dispatch job. On a\n"+
		"pull-request job there is no fix that keeps the secret — the work has to move to\n"+
		"a credential-free check, or off the pull-request path.\n\n"+
		"Which secrets are environment-scoped comes from envreq.E2ERequirements, not from\n"+
		"these files.\n")
	return fmt.Errorf("workflow-secret-scope: %d job(s) cannot resolve an environment-scoped secret", len(findings))
}

// headline is the one-line annotation cause.
func (f Finding) headline() string {
	if f.Kind == PRReachable {
		return "it can run on a pull request, where the infra-<env> environment is locked to main"
	}
	return "the job declares no `environment:`, so the value arrives empty"
}

// detail names what IS present, not only what is missing: for the pull-request
// arm the job usually looks correct, and the reader needs the trigger that
// reaches it — which may be in another file.
func (f Finding) detail() string {
	if f.Kind == PRReachable {
		s := "reachable from a pull request"
		if f.Via != "" {
			s += " via " + f.Via
		}
		if f.Env != "" {
			s += "; declares environment " + f.Env + ", which is locked to ref=main"
		} else {
			s += "; declares no environment"
		}
		return s
	}
	return "declares no `environment:`; these secrets resolve only inside infra-<deployment>"
}
