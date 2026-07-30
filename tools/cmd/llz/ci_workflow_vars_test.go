package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// varsFixture writes a workflow tree reading exactly the variables named.
func varsFixture(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	var b strings.Builder
	b.WriteString("jobs:\n  j:\n    steps:\n")
	for _, n := range names {
		b.WriteString("      - run: echo ${{ vars." + n + " }}\n")
	}
	writeFile(t, filepath.Join(root, "instance-template/.github/workflows/llz-terraform.yml"), b.String())
	return root
}

// Every variable the REAL shipped workflows read must be accounted for. This is
// the gate's own subject matter, asserted against the tree rather than a fixture.
func TestWorkflowVarsRepoIsFullyAccounted(t *testing.T) {
	const root = "../../.." // tests run in tools/cmd/llz
	var out, errOut strings.Builder
	if err := runWorkflowVars(root, &out, &errOut); err != nil {
		t.Fatalf("the repo's own workflow variables must all be accounted for: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "all accounted for") {
		t.Errorf("unexpected report: %s", out.String())
	}
}

// The failure this gate exists for: a workflow starts reading a variable and
// nobody adds it to a requirements list, so `llz doctor` never mentions it and
// the operator meets it as an empty value mid-run.
func TestWorkflowVarsCatchesAnUndeclaredVariable(t *testing.T) {
	root := varsFixture(t, "TF_IMAGE", "SOME_NEW_REQUIRED_THING")
	var errOut strings.Builder
	err := runWorkflowVars(root, io.Discard, &errOut)
	if err == nil {
		t.Fatal("a variable in neither list must fail the gate")
	}
	if !strings.Contains(errOut.String(), "SOME_NEW_REQUIRED_THING") {
		t.Errorf("the report must name the unaccounted variable:\n%s", errOut.String())
	}
	// The remedy must name both lists — which one applies is the author's call.
	if !strings.Contains(errOut.String(), "e2eRequirements") ||
		!strings.Contains(errOut.String(), "knownOptionalWorkflowVars") {
		t.Errorf("the remedy must offer both lists:\n%s", errOut.String())
	}
	// And point at where it is read, or the author has to go hunting.
	if !strings.Contains(errOut.String(), "llz-terraform.yml") {
		t.Errorf("the report must name the reading file:\n%s", errOut.String())
	}
}

// Being listed as deliberately-optional is the other legitimate answer, and must
// satisfy the gate without doctor nagging about it.
func TestWorkflowVarsAcceptsAKnownOptionalVariable(t *testing.T) {
	if len(knownOptionalWorkflowVars) == 0 {
		t.Skip("no known-optional variables declared")
	}
	// Read every accounted variable — both lists — so the only thing under test is
	// that the optional ones satisfy the gate rather than being flagged.
	root := varsFixture(t, append(declaredNamesForTest(), sortedNames(knownOptionalWorkflowVars)...)...)
	if err := runWorkflowVars(root, io.Discard, io.Discard); err != nil {
		t.Errorf("known-optional variables must satisfy the gate: %v", err)
	}
}

// The other direction: a variable nobody reads any more is toil an operator is
// still being asked to configure.
func TestWorkflowVarsCatchesAStaleAccountedVariable(t *testing.T) {
	root := varsFixture(t, "TF_IMAGE") // reads only one of the declared set
	var errOut strings.Builder
	err := runWorkflowVars(root, io.Discard, &errOut)
	if err == nil {
		t.Fatal("declared variables no workflow reads must fail the gate")
	}
	if !strings.Contains(errOut.String(), "no longer read by any workflow") {
		t.Errorf("the report must explain staleness:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "TF_STATE_BUCKET") {
		t.Errorf("the report should name a declared-but-unread variable:\n%s", errOut.String())
	}
}

// Secrets resolve through `secrets: inherit` and environment scoping, so a
// name-only scan cannot judge them. They must not be swept into this gate.
func TestWorkflowVarsIgnoresSecrets(t *testing.T) {
	root := varsFixture(t, declaredNamesForTest()...)
	for n := range knownOptionalWorkflowVars {
		appendFile(t, filepath.Join(root, "instance-template/.github/workflows/llz-terraform.yml"),
			"      - run: echo ${{ vars."+n+" }}\n")
	}
	appendFile(t, filepath.Join(root, "instance-template/.github/workflows/llz-terraform.yml"),
		"      - run: echo ${{ secrets.SOME_UNDECLARED_SECRET }}\n")
	if err := runWorkflowVars(root, io.Discard, io.Discard); err != nil {
		t.Errorf("secrets must be out of scope for this gate: %v", err)
	}
}

// A gate that passes because it found nothing is worse than no gate.
func TestWorkflowVarsRefusesToPassVacuously(t *testing.T) {
	err := runWorkflowVars(t.TempDir(), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("an empty tree must be an error, not a pass")
	}
	if !strings.Contains(err.Error(), "vacuously") {
		t.Errorf("error should explain why, got %v", err)
	}
}

// Every optional entry must carry a REASON, not just a name — the value is what
// tells the next reader whether "unset" is still correct.
func TestKnownOptionalWorkflowVarsCarryReasons(t *testing.T) {
	for name, reason := range knownOptionalWorkflowVars {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("optional variable %q has no reason recorded", name)
		}
	}
}

// A variable must not be in both lists: doctor would report it as required while
// the optional list says unset is fine.
func TestNoVariableIsBothRequiredAndOptional(t *testing.T) {
	for name := range declaredWorkflowVars() {
		if _, ok := knownOptionalWorkflowVars[name]; ok {
			t.Errorf("%q is in both e2eRequirements and knownOptionalWorkflowVars", name)
		}
	}
}

func TestWorkflowVarsCommandWiring(t *testing.T) {
	var found bool
	for _, c := range ciCmd().Commands() {
		if c.Name() == "workflow-vars" {
			found = true
		}
	}
	if !found {
		t.Error("`llz ci workflow-vars` is not registered")
	}
}

func declaredNamesForTest() []string {
	return sortedNames(declaredWorkflowVars())
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}
