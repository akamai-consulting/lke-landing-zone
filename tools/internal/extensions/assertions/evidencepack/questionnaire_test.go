package evidencepack

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "..")
}

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// tableRows returns {N: row text} for every `| N | … |` row in the section of
// doc that starts at heading and ends at the next `## `.
func tableRows(t *testing.T, doc, heading string) map[int]string {
	t.Helper()
	i := strings.Index(doc, heading)
	if i < 0 {
		t.Fatalf("heading %q not found", heading)
	}
	sec := doc[i+len(heading):]
	if j := strings.Index(sec, "\n## "); j >= 0 {
		sec = sec[:j]
	}
	rows := map[int]string{}
	re := regexp.MustCompile(`(?m)^\|\s*(\d+)\s*\|.*$`)
	for _, m := range re.FindAllStringSubmatch(sec, -1) {
		n, _ := strconv.Atoi(m[1])
		rows[n] = m[0]
	}
	return rows
}

// The answers cover EXACTLY the checklist's requirements — the checklist is the
// spine, and a row added there must be answered here (and vice versa).
func TestQuestionnaireAnswersEveryChecklistRow(t *testing.T) {
	rows := tableRows(t, readRepo(t, "docs/infosec/linode-account-request-checklist.md"), "## Step 5")
	if len(rows) == 0 {
		t.Fatal("parsed no rows from the checklist — the parser, not the answers, is broken")
	}
	seen := map[int]bool{}
	for _, r := range Questionnaire {
		if seen[r.Num] {
			t.Errorf("requirement %d answered twice", r.Num)
		}
		seen[r.Num] = true
		if _, ok := rows[r.Num]; !ok {
			t.Errorf("answer for #%d (%s) has no row in the checklist", r.Num, r.Title)
		}
		if r.LLZ == "" && r.Instance == "" && r.Adopter == "" {
			t.Errorf("#%d says nobody satisfies it", r.Num)
		}
	}
	for n := range rows {
		if !seen[n] {
			t.Errorf("checklist row %d has no pre-filled answer", n)
		}
	}
}

// Every cited path exists. A rename that leaves an answer pointing at nothing is
// the stale-link class this PR's own first version had (llz-node-firewall,
// llz-pool, a retired cred-audit).
func TestQuestionnaireRefsExist(t *testing.T) {
	for _, r := range Questionnaire {
		for _, ref := range r.Refs {
			if _, err := os.Stat(filepath.Join(repoRoot(t), filepath.FromSlash(ref))); err != nil {
				t.Errorf("#%d cites %s, which does not exist", r.Num, ref)
			}
		}
	}
}

// SPLIT CONTRACT: the auditor-facing doc and the pack carry the same answers. The
// doc's Section A row for each requirement must cite every path the pack cites
// and every gate it names — both directions read the real artifacts.
func TestControlMappingDocCitesWhatThePackCites(t *testing.T) {
	rows := tableRows(t, readRepo(t, "docs/infosec/control-mapping.md"), "## Section A")
	for _, r := range Questionnaire {
		row, ok := rows[r.Num]
		if !ok {
			t.Errorf("control-mapping.md Section A has no row %d (%s)", r.Num, r.Title)
			continue
		}
		for _, ref := range r.Refs {
			if !strings.Contains(row, ref) {
				t.Errorf("control-mapping.md row %d does not cite %s", r.Num, ref)
			}
		}
		for _, g := range r.Gates {
			if !strings.Contains(row, "`llz ci "+g+"`") {
				t.Errorf("control-mapping.md row %d does not name gate `llz ci %s`", r.Num, g)
			}
		}
		if r.Gap != "" && !strings.Contains(row, "Gap") {
			t.Errorf("control-mapping.md row %d omits the gap the pack states: %s", r.Num, r.Gap)
		}
	}
}

// THE "HARDCODED" CLAIMS ARE PINNED TO THE FILES THEY DESCRIBE. Each answer that
// says LLZ enforces a value unconditionally is a promise to an approver; if the
// value becomes a variable or disappears, the answer is false and must change
// with it.
func TestHardcodedClaimsHold(t *testing.T) {
	claims := []struct {
		num        int
		file, want string
	}{
		{3, "terraform-modules/llz-cluster/firewall.tf", `inbound_policy  = "DROP"`},
		{10, "terraform-modules/llz-cluster/main.tf", `tier        = "enterprise"`},
		{12, "terraform-modules/llz-cluster/main.tf", "acl {\n      enabled = true"},
		{13, "tools/internal/shared/tfroots/roots/cluster/main.tf", `disk_encryption = "enabled"`},
	}
	for _, c := range claims {
		if !strings.Contains(readRepo(t, c.file), c.want) {
			t.Errorf("answer #%d claims %s hardcodes %q — it no longer does; fix the answer and the doc", c.num, c.file, c.want)
		}
	}
	// #3's gap is real until the ICMP rule is narrowed; when it is, drop the gap.
	if strings.Contains(readRepo(t, "terraform-modules/llz-cluster/firewall.tf"), `label = "allow-icmp", protocol = "ICMP", ports = null, ipv4 = ["0.0.0.0/0"]`) {
		if q := find(3); q.Gap == "" {
			t.Error("#3 must state the open-world ICMP rule as a gap while the firewall ships it")
		}
	} else if q := find(3); strings.Contains(q.Gap, "ICMP") {
		t.Error("the open-world ICMP rule is gone — remove it from #3's gap")
	}
}

func find(n int) Requirement {
	for _, r := range Questionnaire {
		if r.Num == n {
			return r
		}
	}
	return Requirement{}
}
