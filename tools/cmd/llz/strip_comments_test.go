package main

import (
	"strings"
	"testing"
)

func TestStripYAMLComments(t *testing.T) {
	in := `# Header line 1
# Header line 2
name: demo

# a top-level YAML comment to strip
on:
  workflow_call:
jobs:
  build:
    # a step-list comment to strip
    steps:
      - name: echo markdown + bash
        run: |
          # this bash comment is LITERAL — keep it
          echo "## Preview" >> "$GITHUB_STEP_SUMMARY"
          cat <<EOF
          # a literal heredoc line — keep it
          EOF
      - name: folded
        run: >
          # inside a folded scalar — keep it
          real content
      - name: after-scalar
        # comment after a scalar block — strip it
        run: echo done
`
	out := stripYAMLComments(in, true)

	mustKeep := []string{
		"# Header line 1", "# Header line 2", // header preserved
		"# this bash comment is LITERAL — keep it",
		`echo "## Preview"`,
		"# a literal heredoc line — keep it",
		"# inside a folded scalar — keep it",
		"real content",
		"echo done",
	}
	for _, s := range mustKeep {
		if !strings.Contains(out, s) {
			t.Errorf("stripped a line it must keep: %q\n---\n%s", s, out)
		}
	}
	mustStrip := []string{
		"# a top-level YAML comment to strip",
		"# a step-list comment to strip",
		"# comment after a scalar block — strip it",
	}
	for _, s := range mustStrip {
		if strings.Contains(out, s) {
			t.Errorf("failed to strip YAML comment: %q\n---\n%s", s, out)
		}
	}
	// Idempotent.
	if again := stripYAMLComments(out, true); again != out {
		t.Errorf("strip is not idempotent:\n%s\n---\n%s", out, again)
	}
}

func TestStripYAMLComments_NoHeader(t *testing.T) {
	in := "# header\nname: x\n# strip me\non: push\n"
	out := stripYAMLComments(in, false)
	if strings.Contains(out, "# header") || strings.Contains(out, "# strip me") {
		t.Errorf("keepHeader=false should strip all YAML comments, got:\n%s", out)
	}
	if !strings.Contains(out, "name: x") || !strings.Contains(out, "on: push") {
		t.Errorf("stripped real content:\n%s", out)
	}
}

func TestStripYAMLComments_TerraformHeredoc(t *testing.T) {
	in := `# file header — kept
variable "x" {
  # a real HCL comment — strip
  type = string
  validation {
    error_message = <<-EOM
      # this is heredoc TEXT (an error message), not a comment — KEEP
      Invalid value.
    EOM
  }
}
`
	out := stripYAMLComments(in, true)
	if !strings.Contains(out, "# this is heredoc TEXT") {
		t.Errorf("stripped a heredoc-body line:\n%s", out)
	}
	if !strings.Contains(out, "# file header — kept") {
		t.Errorf("header not kept:\n%s", out)
	}
	if strings.Contains(out, "# a real HCL comment — strip") {
		t.Errorf("failed to strip a real HCL comment:\n%s", out)
	}
}

func TestCollapseBlankLines(t *testing.T) {
	if got := collapseBlankLines("a\n\n\n\nb\n"); got != "a\n\nb\n" {
		t.Errorf("collapseBlankLines = %q", got)
	}
}
