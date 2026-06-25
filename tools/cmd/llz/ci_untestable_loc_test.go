package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCountRunBlockLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "single-line run is glue, not counted",
			in: "" +
				"      - name: probe\n" +
				"        run: llz ci bao-status\n",
			want: 0,
		},
		{
			name: "block scalar counts non-blank non-comment body lines",
			in: "" +
				"      - name: do\n" +
				"        run: |\n" +
				"          set -euo pipefail\n" +
				"          kubectl get pvc -A\n" +
				"          echo done\n",
			want: 3,
		},
		{
			name: "blank and comment lines skipped",
			in: "" +
				"        run: |\n" +
				"          # a comment\n" +
				"          set -e\n" +
				"\n" +
				"          # another\n" +
				"          echo hi\n",
			want: 2,
		},
		{
			name: "backslash continuation collapses to one logical line",
			in: "" +
				"        run: |\n" +
				"          llz ci bao-seed --path secret/x \\\n" +
				"            --field a=literal:1 \\\n" +
				"            --field b=literal:2\n",
			want: 1,
		},
		{
			name: "two wrapped commands count as two",
			in: "" +
				"        run: |\n" +
				"          llz ci foo --a \\\n" +
				"            --b\n" +
				"          llz ci bar --c \\\n" +
				"            --d\n",
			want: 2,
		},
		{
			name: "block ends when indentation returns to step level",
			in: "" +
				"      - name: one\n" +
				"        run: |\n" +
				"          echo a\n" +
				"          echo b\n" +
				"      - name: two\n" +
				"        uses: actions/checkout@v4\n",
			want: 2,
		},
		{
			name: "two run blocks in one doc both counted",
			in: "" +
				"      - name: one\n" +
				"        run: |\n" +
				"          echo a\n" +
				"      - name: two\n" +
				"        run: |\n" +
				"          echo b\n" +
				"          echo c\n",
			want: 3,
		},
		{
			name: "run: > folded scalar also counted",
			in: "" +
				"        run: >\n" +
				"          echo a\n" +
				"          echo b\n",
			want: 2,
		},
		{
			name: "comment between continued lines does not undercount",
			in: "" +
				"        run: |\n" +
				"          echo a\n" +
				"          # note\n" +
				"          echo b\n",
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countRunBlockLines(tt.in); got != tt.want {
				t.Errorf("countRunBlockLines() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCountScriptLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"only comments and blanks", "#!/bin/bash\n# c\n\n  # indented comment\n", 0},
		{"code lines", "set -e\nx=1\necho $x\n", 3},
		{"trailing-comment line still counts", "x=1  # inline comment\n", 1},
		{"indented code counts", "  if true; then\n    echo hi\n  fi\n", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countScriptLines(tt.in); got != tt.want {
				t.Errorf("countScriptLines() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCountTerraformProvisionerLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "single-line command is glue, not counted",
			in: "" +
				"  provisioner \"local-exec\" {\n" +
				"    command = \"${path.module}/scripts/apply.sh\"\n" +
				"  }\n",
			want: 0,
		},
		{
			name: "indent-stripping heredoc counts body logic lines",
			in: "" +
				"  provisioner \"local-exec\" {\n" +
				"    interpreter = [\"bash\", \"-c\"]\n" +
				"    command     = <<-EOT\n" +
				"      set -euo pipefail\n" +
				"      kubectl wait --for=condition=Ready pod -A\n" +
				"      echo done\n" +
				"    EOT\n" +
				"  }\n",
			want: 3,
		},
		{
			name: "plain heredoc (no dash) with column-0 terminator",
			in: "" +
				"    command = <<EOT\n" +
				"echo a\n" +
				"echo b\n" +
				"EOT\n",
			want: 2,
		},
		{
			name: "blank and comment lines skipped",
			in: "" +
				"    command = <<-EOT\n" +
				"      # prep\n" +
				"      set -e\n" +
				"\n" +
				"      # go\n" +
				"      kubectl apply -f x.yaml\n" +
				"    EOT\n",
			want: 2,
		},
		{
			name: "backslash continuation collapses to one logical line",
			in: "" +
				"    command = <<-EOT\n" +
				"      kubectl patch ns x \\\n" +
				"        --type=merge \\\n" +
				"        -p '{}'\n" +
				"    EOT\n",
			want: 1,
		},
		{
			name: "comment between continued lines does not undercount",
			in: "" +
				"    command = <<-EOT\n" +
				"      echo a\n" +
				"      # note\n" +
				"      echo b\n" +
				"    EOT\n",
			want: 2,
		},
		{
			name: "two command heredocs in one file both counted",
			in: "" +
				"    command = <<-EOT\n" +
				"      echo a\n" +
				"    EOT\n" +
				"    command = <<-EOT\n" +
				"      echo b\n" +
				"      echo c\n" +
				"    EOT\n",
			want: 3,
		},
		{
			name: "non-command heredocs (description/value/policy) are ignored",
			in: "" +
				"  description = <<-EOT\n" +
				"    human prose, not bash\n" +
				"    more prose\n" +
				"  EOT\n" +
				"  rotator_policy = <<-EOT\n" +
				"    path \"x\" { capabilities = [\"read\"] }\n" +
				"  EOT\n",
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countTerraformProvisionerLines(tt.in); got != tt.want {
				t.Errorf("countTerraformProvisionerLines() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCountEmbeddedShellLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "configmap .sh data key counts body, skips shebang/comments/blanks",
			in: "" +
				"data:\n" +
				"  relabel.sh: |\n" +
				"    #!/bin/sh\n" +
				"    # a comment\n" +
				"    set -eu\n" +
				"\n" +
				"    echo hi\n",
			want: 2,
		},
		{
			name: "argo script.source detected by shebang, not key name",
			in: "" +
				"      script:\n" +
				"        command: [\"/bin/sh\"]\n" +
				"        source: |\n" +
				"          #!/bin/sh\n" +
				"          kubectl get pods\n" +
				"          echo done\n",
			want: 2,
		},
		{
			name: "block ends when indentation returns to key level",
			in: "" +
				"  setup.sh: |\n" +
				"    echo a\n" +
				"    echo b\n" +
				"  other: value\n",
			want: 2,
		},
		{
			name: "non-shell block scalar ignored (no .sh key, no shebang)",
			in: "" +
				"  config.yaml: |\n" +
				"    server:\n" +
				"      port: 8080\n" +
				"      host: 0.0.0.0\n",
			want: 0,
		},
		{
			name: "folded prose block ignored",
			in: "" +
				"  description: >\n" +
				"    human prose here\n" +
				"    spanning lines\n",
			want: 0,
		},
		{
			name: "two embedded shell blocks both counted",
			in: "" +
				"  a.sh: |\n" +
				"    echo a\n" +
				"  b.sh: |\n" +
				"    echo b\n" +
				"    echo c\n",
			want: 3,
		},
		{
			name: "block indicators |- and |2 handled",
			in: "" +
				"  trim.sh: |-\n" +
				"    echo a\n" +
				"  keep.sh: |2\n" +
				"    echo b\n",
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countEmbeddedShellLines(tt.in); got != tt.want {
				t.Errorf("countEmbeddedShellLines() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{".github/workflows/*.yml", ".github/workflows/lint.yml", true},
		{".github/workflows/*.yml", ".github/workflows/sub/x.yml", false},
		{"template-scripts/**/*.sh", "template-scripts/lib.sh", true},
		{"template-scripts/**/*.sh", "template-scripts/ci/install.sh", true},
		{"template-scripts/**/*.sh", "template-scripts/a/b/c/x.sh", true},
		{"template-scripts/**/*.py", "template-scripts/x.go", false},
		{"instance-template/.github/actions/**/action.yml", "instance-template/.github/actions/x/action.yml", true},
		{"instance-template/.github/actions/**/action.yml", "instance-template/.github/actions/x/y/action.yml", true},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"x?.sh", "x1.sh", true},
		{"x?.sh", "x12.sh", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"~"+tt.path, func(t *testing.T) {
			if got := matchGlob(tt.pattern, tt.path); got != tt.want {
				t.Errorf("matchGlob(%q,%q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

// TestScanUntestable_EndToEnd builds a tiny fake repo and checks the tallies,
// exclusion, and the over-budget verdict.
func TestScanUntestable_EndToEnd(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(".github/workflows/a.yml", ""+
		"steps:\n"+
		"  - run: |\n"+
		"      echo a\n"+
		"      echo b\n"+
		"  - run: llz ci foo\n") // single-line glue → 0
	write("scripts/logic.sh", "set -e\nx=1\n# comment\necho $x\n")    // 3
	write("scripts/install-thing.sh", "curl -L x | tar xz\nmv a b\n") // excluded
	write("scripts/tool.py", "import os\nprint(os.getcwd())\n")       // 2
	write("scripts/ignored.txt", "not code\n")                        // not matched
	write("infra/main.tf", ""+
		"resource \"null_resource\" \"x\" {\n"+
		"  provisioner \"local-exec\" {\n"+
		"    command = <<-EOT\n"+
		"      set -e\n"+
		"      kubectl apply -f x.yaml\n"+
		"    EOT\n"+
		"  }\n"+
		"  provisioner \"local-exec\" {\n"+
		"    command = \"./glue.sh\"\n"+ // single-line glue → 0
		"  }\n"+
		"}\n") // 2
	write("charts/cm.yaml", ""+ // embedded-shell → 2
		"data:\n"+
		"  relabel.sh: |\n"+
		"    #!/bin/sh\n"+
		"    set -eu\n"+
		"    echo hi\n")

	cfg := untestableBudget{
		Categories: map[string]untestableCategory{
			"wf":  {Kind: "workflow-run", Budget: 1, Include: []string{".github/workflows/*.yml"}},
			"sh":  {Kind: "script", Budget: 10, Include: []string{"scripts/**/*.sh"}},
			"py":  {Kind: "script", Budget: 10, Include: []string{"scripts/**/*.py"}},
			"tf":  {Kind: "terraform-provisioner", Budget: 10, Include: []string{"infra/**/*.tf"}},
			"emb": {Kind: "embedded-shell", Budget: 10, Include: []string{"charts/**/*.yaml"}},
		},
		Exclude: []string{"scripts/install-*.sh"},
	}

	results, err := scanUntestable(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]categoryResult{}
	for _, r := range results {
		got[r.name] = r
	}
	if got["wf"].total != 2 {
		t.Errorf("wf total = %d, want 2", got["wf"].total)
	}
	if !got["wf"].over() {
		t.Errorf("wf should be over budget (2 > 1)")
	}
	if got["sh"].total != 3 {
		t.Errorf("sh total = %d, want 3 (install-*.sh excluded)", got["sh"].total)
	}
	if got["py"].total != 2 {
		t.Errorf("py total = %d, want 2", got["py"].total)
	}
	if got["tf"].total != 2 {
		t.Errorf("tf total = %d, want 2 (single-line command glue not counted)", got["tf"].total)
	}
	if got["emb"].total != 2 {
		t.Errorf("emb total = %d, want 2 (embedded shell body, shebang excluded)", got["emb"].total)
	}
	if got["sh"].over() || got["py"].over() || got["tf"].over() || got["emb"].over() {
		t.Errorf("sh/py/tf/emb should be within budget")
	}
}

func TestLoadUntestableBudget(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, ".untestable-budget.yaml")
	if err := os.WriteFile(p, []byte("categories:\n  sh:\n    kind: script\n    budget: 5\n    include: [\"*.sh\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadUntestableBudget(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Categories["sh"].Budget != 5 || cfg.Categories["sh"].Kind != "script" {
		t.Errorf("unexpected parse: %+v", cfg.Categories["sh"])
	}

	if _, err := loadUntestableBudget(filepath.Join(root, "nope.yaml")); err == nil {
		t.Error("expected error for missing config")
	}

	empty := filepath.Join(root, "empty.yaml")
	_ = os.WriteFile(empty, []byte("exclude: []\n"), 0o644)
	if _, err := loadUntestableBudget(empty); err == nil {
		t.Error("expected error for config with no categories")
	}
}
