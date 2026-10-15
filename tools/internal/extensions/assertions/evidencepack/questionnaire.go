package evidencepack

// questionnaire.go — the answered org security checklist, as INSTANCE DATA.
//
// WHY IT IS A FILE, NOT CODE. Every org has its own checklist: its own row
// numbering, its own intake process, its own CSPM vendor, its own training and
// attribution rules. Compiled into `llz`, one org's answers would ship in every
// adopter's evidence pack. So the engine (harvest, gate-outcome recording, the
// sha256 sidecar) stays here and the answers live in the instance repo, at
// DefaultQuestionnairePath unless --questionnaire says otherwise. The template
// ships a generic, org-neutral starting point
// (instance-template/infosec/questionnaire.yaml.example); docs/infosec/
// control-mapping.md documents the format.
//
// SCHEMA-VALIDATED, FAIL CLOSED. The file is strict-decoded (an unknown or
// misspelled key is an error, not a silently dropped answer) and then validated
// (ValidateQuestionnaire): a known apiVersion/kind, at least one requirement,
// unique positive row numbers, a title on every row, at least one party that
// satisfies it, and well-formed refs/gates. A questionnaire that does not
// validate stops the pack — an evidence bundle that quietly omitted half the
// answers would be worse than none. An ABSENT file is not an error: the pack is
// still built and says, in the pack, that no questionnaire was provided.
//
// Each row splits the requirement by WHO satisfies it:
//
//   - llz      — enforced by the template on every instance, no adopter action;
//   - instance — a spec/tfvars/secret value the instance must set;
//   - adopter  — organizational process no template can reach;
//   - gap      — a known shortfall against the requirement as written. Gaps are
//     answers too: an approval granted on an overstated answer is worse than a
//     recorded exception.
//
// `gates` names `llz ci` verbs whose outcome the pack RECORDS for that row when
// the scheduled job passes it with --gate. A cited gate with no recorded outcome
// renders "not run" — never passed — so cite only gates the job that builds the
// pack actually runs, and name build/bootstrap-time gates in the prose instead.

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Questionnaire file identity.
const (
	QuestionnaireAPIVersion = "llz.akamai-consulting.io/v1alpha1"
	QuestionnaireKind       = "EvidenceQuestionnaire"
	// DefaultQuestionnairePath is where the scheduled job looks, relative to the
	// instance checkout.
	DefaultQuestionnairePath = "infosec/questionnaire.yaml"
)

// Questionnaire is one instance's answered checklist.
type Questionnaire struct {
	APIVersion string                `json:"apiVersion"`
	Kind       string                `json:"kind"`
	Metadata   QuestionnaireMetadata `json:"metadata"`
	// Requirements are the answered rows, in checklist order.
	Requirements []Requirement `json:"requirements"`
}

// QuestionnaireMetadata identifies the checklist being answered.
type QuestionnaireMetadata struct {
	// Name identifies the questionnaire (lowercase kebab).
	Name string `json:"name"`
	// Source names the checklist the rows answer, as the approver knows it.
	Source string `json:"source,omitempty"`
}

// Requirement is one row of the checklist, answered.
type Requirement struct {
	Num   int    `json:"num"`
	Title string `json:"title"`
	// LLZ is what the template enforces by default ("" = nothing).
	LLZ string `json:"llz,omitempty"`
	// Instance is what the instance must configure ("" = nothing).
	Instance string `json:"instance,omitempty"`
	// Adopter is the organizational process that remains ("" = none).
	Adopter string `json:"adopter,omitempty"`
	// Gap is a known shortfall against the requirement as written ("" = none).
	Gap string `json:"gap,omitempty"`
	// Refs are paths (template- or instance-relative) implementing or documenting
	// the control. Free text to the engine; the template's own example is checked
	// against the template tree by its tests.
	Refs []string `json:"refs,omitempty"`
	// Gates are `llz ci` verbs whose recorded outcome is evidence for this row.
	Gates []string `json:"gates,omitempty"`
}

var (
	qNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	qVerbRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// ParseQuestionnaire strict-decodes and validates a questionnaire file's bytes.
// knownVerbs, when non-nil, is the set of registered `llz ci` verbs every cited
// gate must belong to.
func ParseQuestionnaire(raw []byte, knownVerbs map[string]bool) (*Questionnaire, error) {
	var q Questionnaire
	if err := yaml.UnmarshalStrict(raw, &q); err != nil {
		return nil, fmt.Errorf("questionnaire: %w", err)
	}
	if errs := ValidateQuestionnaire(&q, knownVerbs); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return nil, fmt.Errorf("questionnaire is invalid:\n  - %s", strings.Join(msgs, "\n  - "))
	}
	return &q, nil
}

// LoadQuestionnaire reads path through read. A missing file returns (nil, nil):
// the pack records that no questionnaire was provided. Any other failure —
// unreadable, malformed, invalid — is an error.
func LoadQuestionnaire(read func(string) ([]byte, error), path string, knownVerbs map[string]bool) (*Questionnaire, error) {
	raw, err := read(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read questionnaire %s: %w", path, err)
	}
	q, err := ParseQuestionnaire(raw, knownVerbs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return q, nil
}

// ValidateQuestionnaire returns every schema violation in q (nil when valid).
func ValidateQuestionnaire(q *Questionnaire, knownVerbs map[string]bool) []error {
	var errs []error
	if q.APIVersion != QuestionnaireAPIVersion {
		errs = append(errs, fmt.Errorf("apiVersion %q, want %q", q.APIVersion, QuestionnaireAPIVersion))
	}
	if q.Kind != QuestionnaireKind {
		errs = append(errs, fmt.Errorf("kind %q, want %q", q.Kind, QuestionnaireKind))
	}
	if !qNameRe.MatchString(q.Metadata.Name) {
		errs = append(errs, fmt.Errorf("metadata.name %q must be lowercase kebab", q.Metadata.Name))
	}
	if len(q.Requirements) == 0 {
		errs = append(errs, errors.New("requirements is empty — a questionnaire that answers nothing is not evidence"))
	}
	seen := map[int]bool{}
	for i, r := range q.Requirements {
		at := fmt.Sprintf("requirements[%d]", i)
		if r.Num <= 0 {
			errs = append(errs, fmt.Errorf("%s.num %d must be a positive row number", at, r.Num))
		} else if seen[r.Num] {
			errs = append(errs, fmt.Errorf("%s.num %d is answered twice", at, r.Num))
		}
		seen[r.Num] = true
		if strings.TrimSpace(r.Title) == "" {
			errs = append(errs, fmt.Errorf("%s (#%d) has no title", at, r.Num))
		}
		if blank(r.LLZ) && blank(r.Instance) && blank(r.Adopter) {
			errs = append(errs, fmt.Errorf("%s (#%d) says nobody satisfies it — set llz, instance or adopter (and state a gap if that is the honest answer)", at, r.Num))
		}
		for _, ref := range r.Refs {
			if blank(ref) || strings.ContainsAny(ref, "\n|") {
				errs = append(errs, fmt.Errorf("%s (#%d) ref %q must be a non-empty single-line path", at, r.Num, ref))
			}
		}
		for _, g := range r.Gates {
			switch {
			case !qVerbRe.MatchString(g):
				errs = append(errs, fmt.Errorf("%s (#%d) gate %q must be a bare `llz ci` verb (e.g. assert-rotation-health)", at, r.Num, g))
			case knownVerbs != nil && !knownVerbs[g]:
				errs = append(errs, fmt.Errorf("%s (#%d) cites `llz ci %s`, which is not a registered verb", at, r.Num, g))
			}
		}
	}
	return errs
}

// CitedGates returns every gate the questionnaire cites, sorted and deduplicated.
func (q *Questionnaire) CitedGates() []string {
	if q == nil {
		return nil
	}
	set := map[string]bool{}
	for _, r := range q.Requirements {
		for _, g := range r.Gates {
			set[g] = true
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
