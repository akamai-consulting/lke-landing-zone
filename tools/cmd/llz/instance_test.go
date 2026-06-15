package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadAnswers(t *testing.T) {
	dir := t.TempDir()
	body := "" +
		"_commit: v0.1.0\n" +
		"_src_path: gh:akamai-consulting/lke-landing-zone\n" +
		"upstream_org: akamai-consulting\n" +
		"instance_repo: my-org/my-instance\n"
	if err := os.WriteFile(filepath.Join(dir, ".copier-answers.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := readAnswers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil {
		t.Fatal("expected answers, got nil")
	}
	if a.Commit != "v0.1.0" || a.UpstreamOrg != "akamai-consulting" || a.InstanceRepo != "my-org/my-instance" {
		t.Errorf("parsed answers: %+v", a)
	}
}

func TestReadAnswersMissingIsNil(t *testing.T) {
	a, err := readAnswers(t.TempDir())
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if a != nil {
		t.Errorf("expected nil answers, got %+v", a)
	}
}
