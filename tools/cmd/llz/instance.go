package main

import (
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// answers mirrors the fields llz reads out of an instance's .copier-answers.yml
// (written by `copier copy`). sigs.k8s.io/yaml converts YAML to JSON, so the
// struct tags are json tags.
type answers struct {
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

// pinnedTemplateRef resolves the template release THIS instance is rendered from,
// read at runtime out of the instance checkout rather than passed in. It is the
// single source for the pin, so nothing downstream can skew from it: the workflows
// used to carry a `template-ref:` input rendered into every caller stub, which made
// the same fact editable in nine places and let TF_IMAGE drift against it silently.
//
// copier's .copier-answers.yml is the authority (`llz upgrade` rewrites it through
// copier itself); .template-version is a legacy fallback for an instance that has
// not upgraded past the stamp yet. "" when neither is present — callers that need a
// concrete ref default to "main".
func pinnedTemplateRef() string {
	if a, _ := readAnswers("."); a != nil {
		if r := strings.TrimSpace(a.Version); r != "" {
			return r
		}
		if r := strings.TrimSpace(a.Commit); r != "" {
			return r
		}
	}
	return templateRefFromStamp()
}

// readAnswers loads .copier-answers.yml from dir (use "." for the current
// instance). Returns nil with no error when the file is absent — callers treat a
// missing answers file as "not inside an instance yet".
func readAnswers(dir string) (*answers, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".copier-answers.yml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var a answers
	if err := yaml.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}
