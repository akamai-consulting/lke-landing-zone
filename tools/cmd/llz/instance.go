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
