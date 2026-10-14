package cli

import (
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/evidencepack"
)

// The evidence pack tells an approver which gate proves each checklist answer.
// A gate that was renamed or retired leaves that answer citing a verb nothing
// runs — the shape of the `cred-audit` reference this PR's first version
// carried for months after the verb was retired. The tree is the authority.
func TestQuestionnaireGatesAreRealVerbs(t *testing.T) {
	registered := map[string]bool{}
	for _, c := range ciCmd().Commands() {
		registered[c.Name()] = true
	}
	cited := 0
	for _, r := range evidencepack.Questionnaire {
		for _, g := range r.Gates {
			cited++
			if !registered[g] {
				t.Errorf("checklist answer #%d cites `llz ci %s`, which is not a registered verb", r.Num, g)
			}
		}
	}
	if cited == 0 {
		t.Fatal("the questionnaire cites no gates at all — this test examined nothing")
	}
}
