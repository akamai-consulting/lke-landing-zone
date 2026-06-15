package main

import (
	"os"
	"path/filepath"

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

// ghHost is the GitHub host used to select the forge backend (forge.go) and build
// token-creation links. It defaults to github.com and is overridable via
// LLZ_GH_HOST for GitHub Enterprise instances (the host is not derivable from the
// <owner>/<name> instance_repo answer). Pairs with the copier forge_flavor =
// github-enterprise scaffold answer.
func ghHost() string {
	if h := os.Getenv("LLZ_GH_HOST"); h != "" {
		return h
	}
	return "github.com"
}
