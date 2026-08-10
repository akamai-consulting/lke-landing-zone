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
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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
	return TemplateRefFromStamp()
}

// fileReader is the one call this package makes against the disk, named so the
// same body can serve a fenced caller and an unfenced one.
//
// capability.Repo satisfies it structurally — no adapter, no import, and no risk
// of the two paths drifting, because there is only one path. That is the whole
// reason ReadFrom is not a second copy of Read: a duplicated parse is how the two
// halves of a guard come to disagree about what a file means.
type fileReader interface {
	ReadFile(string) ([]byte, error)
}

// osReader is the unfenced reader, for the callers outside the capability model.
type osReader struct{}

func (osReader) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

// Read loads .copier-answers.yml from dir (use "." for the current
// instance). Returns nil with no error when the file is absent — callers treat a
// missing File file as "not inside an instance yet".
//
// UNFENCED, AND THAT IS NOT AN OVERSIGHT. Its callers are internal/verbs — the
// CLI's own surface, which declares no bindings at all and is held to that by
// shared/extension/verbs_test.go — plus envdef. A verb scaffolds and resolves
// trees that are not "the repo" (`llz new` writes one that does not exist yet),
// so there is no root to fence it to and a grant it could never hold. Extensions
// use ReadFrom.
func Read(dir string) (*File, error) { return readFrom(osReader{}, dir) }

// ReadFrom is Read through a caller's own reader, so an extension's read of the
// answers file is bounded by the same fence as the rest of its tree. Takes a
// capability.Repo in practice; the narrow interface keeps this package from
// importing the capability layer it sits beside.
func ReadFrom(r fileReader, dir string) (*File, error) { return readFrom(r, dir) }

func readFrom(r fileReader, dir string) (*File, error) {
	b, err := r.ReadFile(filepath.Join(dir, ".copier-answers.yml"))
	if err != nil {
		// errors.Is rather than os.IsNotExist: a FENCED reader refusing a path
		// outside its tree must NOT read as "no answers file here", which every
		// caller treats as "not inside an instance" and shrugs off. A refusal and
		// an absence are different answers, and only one of them is benign.
		if errors.Is(err, fs.ErrNotExist) {
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

// TemplateRefFromStamp reads the template ref out of .template-version.
//
// IT CAME DOWN FROM internal/extensions/promote, which is where it was written
// and where it did not belong: this package is substrate, promote is a
// capability, and a shared package importing an extension inverts the layering
// the boundary test now enforces. Nothing about reading a stamp file is a
// promotion concern — answers already reads the same file for PinnedTemplateRef.
// templateRefFromStamp reads the template_ref out of .template-version (best
// effort; "" if absent/malformed).
// TemplateRefFromStamp reads the template ref recorded in the version stamp.
func TemplateRefFromStamp() string {
	b, err := os.ReadFile(".template-version")
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`"template_ref"\s*:\s*"([^"]+)"`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}
