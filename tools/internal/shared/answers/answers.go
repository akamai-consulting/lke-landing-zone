// Package answers reads an instance's .copier-answers.yml — the file that records
// which template release the instance was rendered from and what it answered.
//
// EXTRACTED AS AN ENABLER, and an honest one: it is the hub FOURTEEN files in
// cmd/llz reached through, and the campaign's own state file had written the
// scaffold mass off as "a design task, no enabler to extract first". That was
// wrong about this file — 65 lines, closure 1, sitting in plain sight.
//
// It was also only PARTLY right to be optimistic. Moving it took template_commit
// from 10 outbound to 8, selfupdate 8 to 7, render 11 to 10 — real, and nothing
// like the collapses ghsecret and ghgitdata produced elsewhere. The remaining
// coupling is not one shared helper; it is main.go's globals and the
// answers/spec/tfvars read path genuinely interleaving. That part IS a design
// task.
package answers

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/promote"
	"sigs.k8s.io/yaml"
)

// File mirrors the fields llz reads out of an instance's .copier-answers.yml
// (written by `copier copy`). sigs.k8s.io/yaml converts YAML to JSON, so the
// struct tags are json tags.
type File struct {
	Commit       string `json:"_commit"`
	SrcPath      string `json:"_src_path"`
	UpstreamOrg  string `json:"upstream_org"`
	InstanceRepo string `json:"instance_repo"`
	Version      string `json:"llz_version"`
	// OpenbaoTeam is the copier-chosen default team name (spec.teams[0]) — the
	// operators who get scoped non-root OpenBao writes. Empty on pre-question
	// instances; ensureLandingZone falls back to "platform".
	OpenbaoTeam string `json:"openbao_team"`
}

// PinnedTemplateRef resolves the template release THIS instance is rendered from,
// read at runtime out of the instance checkout rather than passed in. It is the
// single source for the pin, so nothing downstream can skew from it: the workflows
// used to carry a `template-ref:` input rendered into every caller stub, which made
// the same fact editable in nine places and let TF_IMAGE drift against it silently.
//
// copier's .copier-answers.yml is the authority (`llz upgrade` rewrites it through
// copier itself); .template-version is a legacy fallback for an instance that has
// not upgraded past the stamp yet. "" when neither is present — callers that need a
// concrete ref default to "main".
func PinnedTemplateRef() string {
	if a, _ := Read("."); a != nil {
		if r := strings.TrimSpace(a.Version); r != "" {
			return r
		}
		if r := strings.TrimSpace(a.Commit); r != "" {
			return r
		}
	}
	return promote.TemplateRefFromStamp()
}

// Read loads .copier-answers.yml from dir (use "." for the current
// instance). Returns nil with no error when the file is absent — callers treat a
// missing File file as "not inside an instance yet".
func Read(dir string) (*File, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".copier-answers.yml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var a File
	if err := yaml.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}
