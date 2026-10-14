package cli

// --require-pipeline is a QUESTION, so it needs the question mode. Without the
// guard it also armed the WRITE path: `llz env pipeline --require-pipeline` on a
// single-deployment instance regenerated the empty placeholder, wrote it
// successfully, and then exited 1 to report that the file it had just written
// declares no pipeline. A true statement, delivered as if the write had failed —
// and the one shape an operator following the printed remedy lands in.

import (
	"strings"
	"testing"
)

func TestRequirePipelineNeedsCheck(t *testing.T) {
	c := envPipelineCmd()
	c.SetArgs([]string{"--require-pipeline"})
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.Execute()
	if err == nil {
		t.Fatal("--require-pipeline without --check must be rejected, not run the write path")
	}
	if !strings.Contains(err.Error(), "only applies to --check") {
		t.Errorf("the error must say which mode the flag belongs to; got %v", err)
	}

	// The combination the generated preflight actually uses must still parse. This
	// test would otherwise be satisfied by rejecting the flag outright.
	c = envPipelineCmd()
	if f := c.Flags().Lookup("require-pipeline"); f == nil {
		t.Fatal("--require-pipeline must still exist")
	}
}
