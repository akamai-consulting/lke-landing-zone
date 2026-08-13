package applydrift

// applydrift.go implements `llz ci apply-drift` — has a Terraform change merged
// that this deployment has not applied?
//
// THE GAP IT REPORTS. A merged change reaches an instance's cluster by three
// routes and only two are pull-based: the apl-overlay is git-synced continuously
// by the in-cluster reconciler, and apl-values/<env>/manifest/** is pulled by
// Argo. Terraform lands on NOTHING. A push to main deliberately neither plans nor
// applies (llz-terraform.yml's push-noop-notice exists to say so), and the apply
// is a workflow_dispatch an operator fires. So an upgrade that moves the module
// ref merges, goes green, and sits undeployed with nothing reporting it.
//
// A DETECTOR RATHER THAN AN APPLIER, and that is the design decision, not a
// limitation. promote.yml already walks ranked deployments with `needs:` as a
// green gate, per-stage `environment:` approval, and "Re-run failed jobs" as
// resume. An earlier attempt at this built a second, weaker apply engine beside
// it — fanning out ALPHABETICALLY with fail-fast: false, so it could apply prod
// before staging and would not stop on a failed dev — and needed a PAT, a run
// watcher and a relaxed apply guard to do it. The gap was never "nothing can
// apply"; it was "nobody knows they need to". This says so, and a human dispatches
// the pipeline that already exists.
//
// HOW IT KNOWS WHAT WAS LAST APPLIED. The runs API does not expose a dispatch's
// inputs, so the deployment cannot be read off the run — but llz-terraform.yml
// names its chained job "Bootstrap OpenBao (<deployment>)", and job names ARE
// readable. That is the per-deployment key. Measured, not assumed: both calls
// were run against a live repo before this was written.

import (
	"fmt"
	"io"
	"strings"
)

// TerraformPaths are the trees whose changes only reach a cluster through an
// apply. apl-values/ is deliberately ABSENT: Argo and the in-cluster reconciler
// pull it continuously, so a change there is already live and reporting it as
// drift would train the reader to ignore this check.
var TerraformPaths = []string{
	"terraform-iac-bootstrap/",
	"landingzone.yaml",
	"environments/",
}

// Verdict is what the check found.
type Verdict struct {
	Deployment  string
	AppliedSHA  string   // head_sha of the newest successful apply for this deployment
	Behind      []string // changed paths that only an apply can deliver
	CheckedRuns int
}

// Relevant filters a changed-file list down to what an apply must deliver.
//
// Pure, so the judgement is testable without a repo or a forge — and separate
// from the transport for the same reason the rest of this tree is: the decision
// is the part that can be wrong quietly.
func Relevant(changed []string) []string {
	var out []string
	for _, f := range changed {
		for _, p := range TerraformPaths {
			if (strings.HasSuffix(p, "/") && strings.HasPrefix(f, p)) || f == p {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// Report writes the verdict and returns an error when the deployment is behind.
func (v Verdict) Report(w io.Writer, strict bool) error {
	if len(v.Behind) == 0 {
		fmt.Fprintf(w, "::notice title=%s is up to date::no Terraform or spec change has merged since the last "+
			"successful apply (%s).\n", v.Deployment, short(v.AppliedSHA))
		return nil
	}
	fmt.Fprintf(w, "::%s title=%s is behind::%d change(s) have merged since the last successful apply (%s) that "+
		"only an apply can deliver. Promote with promote.yml, or apply this deployment with `llz build %s --yes`.\n",
		level(strict), v.Deployment, len(v.Behind), short(v.AppliedSHA), v.Deployment)
	for _, f := range v.Behind {
		fmt.Fprintf(w, "    %s\n", f)
	}
	if strict {
		return fmt.Errorf("apply-drift: %s is %d change(s) behind main", v.Deployment, len(v.Behind))
	}
	return nil
}

func level(strict bool) string {
	if strict {
		return "error"
	}
	return "warning"
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
