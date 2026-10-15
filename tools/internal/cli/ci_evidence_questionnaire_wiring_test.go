package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/extensions/assertions/evidencepack"
)

// The evidence pack tells an approver which gate proves each checklist answer.
// A gate that was renamed or retired leaves that answer citing a verb nothing
// runs — the shape of the `cred-audit` reference the pack's first version carried
// for months after the verb was retired. The tree is the authority: the shipped
// example questionnaire must validate against the REAL `llz ci` verb set, the
// same set `cis-evidence` checks an instance's file against at runtime.
func TestExampleQuestionnaireGatesAreRealVerbs(t *testing.T) {
	registered := map[string]bool{}
	for _, c := range ciCmd().Commands() {
		registered[c.Name()] = true
	}
	if !registered["cis-evidence"] {
		t.Fatal("cis-evidence is not registered under `llz ci`")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "instance-template", "infosec", "questionnaire.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	q, err := evidencepack.ParseQuestionnaire(raw, registered)
	if err != nil {
		t.Fatalf("the shipped example does not validate against the registered verbs: %v", err)
	}
	if len(q.CitedGates()) == 0 {
		t.Fatal("the example cites no gates at all — this test examined nothing")
	}
}
