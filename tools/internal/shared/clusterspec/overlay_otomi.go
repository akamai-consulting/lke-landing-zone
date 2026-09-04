package clusterspec

import (
	"bytes"
	"regexp"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// overlay_otomi.go — the apl-core version, rendered as apl-core's own settings CR.
//
// Renders spec.version into env/settings/otomi.yaml, which the reconciler merges
// KEY-LEVEL onto the apl-<env> branch (apl/overlay.otomiOverlayFiles) — apl-core
// co-writes that file, so it is deliberately not the whole-file path obj.yaml
// takes. Off unless spec.cluster.bootstrap.manageAplVersion says otherwise; see
// that field for why the default is not to touch it.

// aplCoreVersionPattern is the pattern apl-core's values schema enforces on
// otomi.version. Restated here so a rendered value that the operator would reject
// fails in this repo's tests rather than in a cluster.
var aplCoreVersionPattern = regexp.MustCompile(`(v[0-9]+.[0-9]+.[0-9]+|[a-zA-Z]+[a-zA-Z0-9-])`)

// OverlayOtomiFile is apl-core's platform-settings file in the values tree.
const OverlayOtomiFile = "otomi.yaml"

// otomiKind is the CR apl-core stores env/settings/otomi.yaml as — confirmed
// against apl-core v6.2.1's own fixture, tests/fixtures/env/settings/otomi.yaml.
const otomiKind = "AplCapabilitySet"

type otomiOverlayDoc struct {
	Kind     string    `yaml:"kind"`
	Metadata otomiMeta `yaml:"metadata"`
	Spec     otomiSpec `yaml:"spec"`
}

type otomiMeta struct {
	Name string `yaml:"name"`
}

// otomiSpec carries ONLY the version, and every other capability in that CR is
// deliberately absent rather than zero-valued.
//
// `spec.git` lives in this same document, and it is apl-core's BYO-Git wiring — the
// repo, branch and username `llz ci bootstrap-cluster` configured. The overlay
// reconciler deep-merges fragments, so emitting only the key LLZ owns leaves the rest
// of the operator's own document standing. A struct without omitempty would write
// `git: {}` and take the platform's git config out from under it.
type otomiSpec struct {
	Version string `yaml:"version,omitempty"`
}

// RenderOtomiOverlayEnv returns the per-env otomi.yaml, or "" when llz is not
// managing the version — in which case no file is written and Linode's version
// stands, which is the default.
func RenderOtomiOverlayEnv(b Bootstrap) string {
	if !b.ManageAplVersion {
		return ""
	}
	v := aplCoreVersionValue(EffectiveAplChartVersion(b.AplChartVersion))
	if v == "" {
		return ""
	}
	return marshalYAML(otomiOverlayDoc{
		Kind:     otomiKind,
		Metadata: otomiMeta{Name: "otomi"},
		Spec:     otomiSpec{Version: v},
	})
}

// aplCoreVersionValue puts a bare semver into the form apl-core's schema accepts.
//
// LLZ TOLERATES A BARE PIN AND apl-core DOES NOT. Every comparison in this package
// goes through AplSemver, which strips a leading "v" — AplBaselineHistory even
// carries "6.0.0" unprefixed — so `aplChartVersion: 6.1.0` is a perfectly ordinary
// spec here. apl-core's values schema is stricter: its pattern admits `v6.1.0` or a
// letter-leading name like `main`, and matches nothing in `6.1.0`. Rendering the
// operator's own spelling straight through would hand the platform a values tree it
// rejects, and the rejection would surface in-cluster rather than at render.
//
// A name (`latest`, `main`) passes through untouched: it is already what the pattern
// permits, and it is not ours to reinterpret.
func aplCoreVersionValue(v string) string {
	if v == "" || v[0] < '0' || v[0] > '9' {
		return v
	}
	return "v" + v
}

// OtomiOverlayVersion reads spec.version out of the rendered otomi overlay source.
// "" (with no error) means the source says nothing, which is how an instance that
// has not opted in reads.
func OtomiOverlayVersion(src []byte) (string, error) {
	m, err := unmarshalMap(src)
	if err != nil {
		return "", err
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		return "", nil
	}
	v, _ := spec["version"].(string)
	return strings.TrimSpace(v), nil
}

// AplCoreVersionAccepted reports whether apl-core's values schema would accept a
// version. UNANCHORED on purpose: JSON Schema `pattern` is a partial match, so this
// mirrors what apl-core actually enforces rather than a stricter rule of our own.
//
// It exists because the pattern had NO production caller — it was checked only in
// this package's tests, on the RENDER half, while the reconciler took a version
// straight from the overlay source to the machine branch unvalidated. A value
// apl-core rejects converges silently: the merge is a no-op on the next pass, so it
// sits there, permanently refused, with nothing red.
func AplCoreVersionAccepted(v string) bool {
	return aplCoreVersionPattern.MatchString(strings.TrimSpace(v))
}

// OtomiCRIsMergeable reports whether a target file is a real apl-core settings CR
// and therefore safe to merge a single key into.
//
// AN EXISTING-BUT-DEGENERATE FILE IS NOT A MERGE BASE. The production
// ghgitdata.ReadFile answers found=true for a file that exists and is EMPTY — only
// a 404 is found=false — so an empty, "{}", "null" or comment-only
// env/settings/otomi.yaml reaches the merge as "present". Merging into nothing
// yields a two-line `spec.version` document with no kind, no metadata and none of
// apl-core's eight settings, pushed over its CR: the `{}`-over-obj.yaml regression
// this whole key-level path exists to avoid, reached through a degenerate target
// instead of an absent one.
//
// Requiring `kind` is the cheap discriminator — every apl-core settings CR carries
// one, and nothing LLZ would want to merge into lacks it.
func OtomiCRIsMergeable(current []byte) bool {
	m, err := unmarshalMap(current)
	if err != nil || len(m) == 0 {
		return false
	}
	kind, _ := m["kind"].(string)
	return strings.TrimSpace(kind) != ""
}

// OtomiTargetIsSingleDocument reports whether a target holds exactly one YAML
// document. A merge re-marshals ONE document, so a multi-document file would come
// back silently truncated to its first — data loss on a file LLZ does not own.
func OtomiTargetIsSingleDocument(current []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(current))
	docs := 0
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if err != nil {
			break
		}
		docs++
		if docs > 1 {
			return false
		}
	}
	return docs == 1
}

// SetOtomiVersion asserts spec.version on apl-core's OWN otomi settings CR,
// leaving every other key untouched.
//
// A KEY-LEVEL MERGE, NOT A FILE WRITE, and on this file that is not a refinement —
// it is the difference between working and destroying the platform's settings.
// apl-core co-writes env/settings/otomi.yaml (its commits read `updated values
// [ci skip]`) and keeps eight other fields there, observed live on a managed
// cluster. LLZ owns exactly one key. A wholesale write would blank the rest.
//
// The CREATE-if-absent model the team CRs use is also wrong here, for the opposite
// reason: that file always exists, so a never-clobber rule would mean the version
// LLZ renders never lands at all — the feature would look wired and do nothing.
//
// Returns changed=false when the version already matches, so the reconciler does
// not push a commit every pass against a file apl-core keeps rewriting.
func SetOtomiVersion(current []byte, want string) (updated []byte, changed bool, err error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return current, false, nil
	}
	m, err := unmarshalMap(current)
	if err != nil {
		return nil, false, err
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		m["spec"] = spec
	}
	if cur, ok := spec["version"].(string); ok && cur == want {
		return current, false, nil // already correct — no push, no re-marshal
	}
	spec["version"] = want
	return marshalMap(m), true, nil
}
