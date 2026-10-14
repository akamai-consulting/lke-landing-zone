package callerperms

// guard.go implements `llz ci reusable-workflow-caller-permissions` — a called
// workflow's jobs can never hold more than the CALLING job's token.
//
// WHY: THE FAILURE IS INVISIBLE BY CONSTRUCTION. When a reusable workflow's job
// asks for a permission the caller does not hold, GitHub does not fail that job —
// it refuses to start the RUN. The result is `startup_failure`: no jobs, no step
// logs, no annotations, and nothing for a notification to hang off. The run
// appears in the list with a red dot and no explanation anywhere inside it.
//
// It had already happened, on the worst possible workflow. The delivered
// `secret-rotation.yml` granted its `call:` job nothing but the workflow-level
// `contents: read`, while the reusable body's `propagate-linode-pat` job asks for
// `id-token: write` (OIDC for OpenBao jwt auth, role secret-propagator). Every
// scheduled run since had died at startup — a CREDENTIAL ROTATION that has never
// once executed, reporting nothing. Loud failure would have been a better outcome
// than what it did instead, which was look like it was configured.
//
// The rule was already known and written down: release-e2e.yml's `lanes` job
// carries a comment explaining that its permissions "must be the union of what
// the lane's jobs ask for" and naming startup_failure explicitly. That is the
// signature this repo keeps rediscovering — a rule that exists in one file's
// prose and is enforced nowhere, so the next author of a different workflow gets
// no warning at all.
//
// SCOPE: local reusable calls (`uses: ./.github/workflows/x.yml`) only. A remote
// `uses: org/repo/.github/workflows/x.yml@ref` cannot be resolved from this tree,
// and guessing at it would be worse than declining — see the note in Run.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/color"
)

// scanRoots are the workflow trees checked. instance-template's is the one that
// matters most: those files are DELIVERED, so a startup_failure there is every
// adopter's, and the one real instance of this bug was in exactly that tree.
var scanRoots = []string{".github/workflows", "instance-template/.github/workflows"}

// localCallPrefix marks a reusable call this guard can resolve.
const localCallPrefix = "./.github/workflows/"

// workflow is the sliver of an Actions file this guard reads.
type workflow struct {
	Permissions any `yaml:"permissions"`
	Jobs        map[string]struct {
		Permissions any    `yaml:"permissions"`
		Uses        string `yaml:"uses"`
	} `yaml:"jobs"`
}

// perms is scope -> level, normalised.
type perms map[string]string

// parsePermissions handles both forms GitHub accepts: a mapping of scopes, and
// the bare strings `read-all` / `write-all`. The string form is not a curiosity —
// missing it would make a caller that holds write-all look like it holds nothing
// and produce a confident false finding.
func parsePermissions(v any) (perms, bool) {
	switch t := v.(type) {
	case string:
		switch strings.TrimSpace(t) {
		case "write-all":
			return perms{"*": "write"}, true
		case "read-all":
			return perms{"*": "read"}, true
		}
		return nil, false
	case map[string]any:
		p := perms{}
		for k, val := range t {
			if s, ok := val.(string); ok {
				p[k] = s
			}
		}
		return p, true
	}
	return nil, false
}

// rank orders the three levels GitHub recognises, so "does the caller cover the
// callee" is a comparison rather than a special case per level.
//
// THIS USED TO BE A holdsWrite BOOLEAN, and the gap it left is the one this guard
// exists to close. GitHub validates the ceiling for EVERY level, not only write:
// a callee job asking `pull-requests: read` under a caller holding
// `contents: read` is `pull-requests: none` vs `read`, which fails the run at
// startup exactly as a write escalation does. Checking only `level == "write"`
// meant a read escalation printed "OK — N local reusable call(s) cover their
// callee" while every dispatch of that pipeline died with no jobs and no logs.
//
// FOUND ON A REAL CHANGE, not by inspection: a feature branch added a job to
// llz-terraform.yml asking for `pull-requests: read` (it wanted the pulls API).
// Every caller of that reusable — terraform.yml's `call:` job and all three
// promote.yml stages — holds `contents: read`, so PR plans, dispatched applies
// and promotions alike would have become startup_failures. This guard, whose
// entire purpose is that class, printed OK.
func rank(level string) int {
	switch level {
	case "write":
		return 2
	case "read":
		return 1
	default:
		return 0
	}
}

// holds reports whether p grants at least want on scope, honouring the
// read-all / write-all wildcards.
func (p perms) holds(scope, want string) bool {
	have := p[scope]
	if w, ok := p["*"]; ok && rank(w) > rank(have) {
		have = w
	}
	return rank(have) >= rank(want)
}

// Finding is one caller job that cannot cover its callee.
type Finding struct {
	File, Job, Callee string
	Missing           []string
}

// Run fails when a local reusable call asks for more than its caller holds.
func Run(root string, out, errOut io.Writer) error {
	repo := capability.RepoForGate(Extension(), root)

	docs := map[string]workflow{} // repo-relative path -> parsed
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
			docs[filepath.ToSlash(p)] = w
			return nil
		})
		if err != nil {
			return fmt.Errorf("reusable-workflow-caller-permissions: %w", err)
		}
	}
	// Fail closed: no workflows is what a wrong --root looks like.
	if len(docs) == 0 {
		return fmt.Errorf("reusable-workflow-caller-permissions: no workflows found under %s — refusing to pass vacuously",
			strings.Join(scanRoots, ", "))
	}

	var findings []Finding
	calls := 0
	for file, w := range docs {
		top, _ := parsePermissions(w.Permissions)
		for jobName, job := range w.Jobs {
			if !strings.HasPrefix(job.Uses, localCallPrefix) {
				continue // remote or not a reusable call — see the header's SCOPE note
			}
			// The callee sits beside the caller in the SAME tree. Resolving it
			// relative to the caller's directory is what makes this work for both
			// the template's own .github/ and instance-template's copy.
			callee := path.Join(path.Dir(file), path.Base(job.Uses))
			cw, ok := docs[callee]
			if !ok {
				continue // not in this tree (rendered elsewhere) — nothing to compare
			}
			calls++

			caller := top
			if jp, ok := parsePermissions(job.Permissions); ok {
				caller = jp // a job-level block REPLACES the workflow-level set
			}

			need := calleeUnion(cw)
			var missing []string
			for scope, level := range need {
				// `none` is not an ask, so it can never be an escalation.
				if rank(level) > 0 && !caller.holds(scope, level) {
					missing = append(missing, fmt.Sprintf("%s: %s", scope, level))
				}
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				findings = append(findings, Finding{File: file, Job: jobName, Callee: callee, Missing: missing})
			}
		}
	}
	if calls == 0 {
		return fmt.Errorf("reusable-workflow-caller-permissions: no local reusable calls found — the check would be " +
			"vacuous, so this is a failure rather than a pass")
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Job < findings[j].Job
	})
	if len(findings) == 0 {
		fmt.Fprintf(out, "reusable-workflow-caller-permissions: OK — %d local reusable call(s) cover their callee\n", calls)
		return nil
	}
	for _, f := range findings {
		fmt.Fprintf(errOut, "::error file=%s::job %q calls %s, whose jobs need %s — the caller does not grant it\n",
			f.File, f.Job, path.Base(f.Callee), strings.Join(f.Missing, ", "))
	}
	fmt.Fprintf(errOut, "\n%s %d reusable call(s) ask for more than the caller holds:\n", color.Red("✗"), len(findings))
	for _, f := range findings {
		fmt.Fprintf(errOut, "    %s  job %q -> %s  MISSING: %s\n", f.File, f.Job, path.Base(f.Callee), strings.Join(f.Missing, ", "))
	}
	fmt.Fprint(errOut, "\nA called workflow's jobs can never hold more than the CALLING job's token, so GitHub\n"+
		"refuses to start the RUN — `startup_failure`, with no jobs, no step logs and no\n"+
		"annotations to explain it. That is how a scheduled credential rotation ran zero\n"+
		"times while looking configured.\n\n"+
		"Fix: give the calling JOB the union of what the callee's jobs ask for (a job-level\n"+
		"`permissions:` block REPLACES the workflow-level one, so list everything it needs,\n"+
		"including `contents: read`).\n")
	return fmt.Errorf("reusable-workflow-caller-permissions: %d call(s) under-granted", len(findings))
}

// calleeUnion is the widest permission each scope reaches across a callee's jobs,
// with a job's own block replacing the workflow-level default.
func calleeUnion(w workflow) perms {
	top, _ := parsePermissions(w.Permissions)
	u := perms{}
	for _, job := range w.Jobs {
		jp := top
		if p, ok := parsePermissions(job.Permissions); ok {
			jp = p
		}
		for scope, level := range jp {
			// BY RANK, NOT BY STRING. `u[scope] != "write"` let a later job's
			// explicit `none` overwrite an earlier job's `read`, so what the union
			// reported depended on Go's map iteration order — the same read level
			// this guard was just taught to check could vanish before it was
			// compared. The union is the WIDEST ask across the callee's jobs.
			if rank(level) > rank(u[scope]) {
				u[scope] = level
			}
		}
	}
	return u
}
