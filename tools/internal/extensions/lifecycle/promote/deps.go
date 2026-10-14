package promote

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/envtopology"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/instancelayout"
)

// Deps carries what this package cannot reach for itself.
//
// All four are READS OF THE INSTANCE REPO, which is the whole shape of this
// extension: it derives a workflow DAG from what the instance already declares.
// The one thing it does that is NOT a read — writing .github/workflows/promote.yml
// — it does directly through os.WriteFile, and that asymmetry is the finding. See
// extension.go.
//
// Deps is THREADED as a parameter, not installed: the call tree is three frames
// and the seams are all at the top of it. Installation stays the exception it was
// for internal/converge.
type Deps struct {
	// Layout returns the instance's terraform / apl directories and the relative
	// prefix between them. Package main owns the repo layout; this package only
	// needs to know where to look.
	Layout func() (tfDir, aplDir, relPrefix string)

	// ListDeployments enumerates the deployments declared in cluster tfvars. The
	// promotion DAG is built from these plus their promotion_rank.
	ListDeployments func(tfDir string) ([]string, error)

	// LoadSpec reads the instance's LandingZone spec.
	LoadSpec func() (*clusterspec.LandingZone, bool, error)

	// InstanceRepo returns the `instance_repo` recorded in .copier-answers.yml, or
	// "" when the file is absent or has no such key.
	//
	// A STRING, NOT THE `answers` STRUCT. This package reads exactly one field of
	// it, and taking the whole type would pull package main's copier-answers model
	// across the boundary to answer a one-line question. The narrowest seam that
	// answers the question is the seam.
	InstanceRepo func() string
}

// DefaultDeps is the PRODUCTION wiring of the four seams above — the instance
// tree as it really sits on disk.
//
// IT EXISTS SO THERE IS EXACTLY ONE OF IT. This was previously built inline in
// internal/cli, where `llz env pipeline` was the only caller and a local literal
// was the right size for the job. It stopped being the only caller when `llz
// doctor` began asking the same question, and a second hand-built Deps is the
// standing invitation for the two to answer it differently — which is precisely
// the defect this whole check exists to catch, committed one layer down. The CI
// gate and the local pre-flight must read the same tfvars, the same spec and the
// same .copier-answers.yml, or a green doctor stops being evidence about CI.
//
// Callers that need to substitute a seam still build a Deps literal; nothing here
// is installed, and Deps stays threaded as a parameter.
func DefaultDeps() Deps {
	return Deps{
		Layout:          instancelayout.Detect,
		ListDeployments: envtopology.ListDeployments,
		LoadSpec:        func() (*clusterspec.LandingZone, bool, error) { return clusterspec.Detected() },
		// Narrowed to the one field this package reads. Handing over the whole
		// `answers` struct would put the copier-answers model on the other side of
		// the boundary to answer a one-line question.
		InstanceRepo: func() string {
			a, _ := answers.Read(".")
			if a == nil {
				return ""
			}
			return a.InstanceRepo
		},
	}
}
