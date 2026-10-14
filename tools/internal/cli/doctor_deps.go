package cli

// doctor_deps.go — wires internal/doctor's capabilities.
//
// Both defaults in that package already do the real thing, so this install is not
// load-bearing for correctness; it exists so the doctor probes read the spec and
// the copier answers through package main's OWN helpers, which know the instance
// layout. Keeping one reader rather than two is the point — a second copy of
// "where is the spec" is exactly the drift .template-manifest and docsguard's
// keep-set both exist to prevent.

import (
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/answers"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/clusterspec"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/verbs/doctor"
)

func init() { installDoctorDeps() }

func installDoctorDeps() {
	doctor.Install(doctor.Deps{
		LoadSpec: clusterspec.Detected,
		InstanceRepo: func() string {
			a, _ := answers.Read(".")
			if a == nil {
				return ""
			}
			return a.InstanceRepo
		},
	})
}
