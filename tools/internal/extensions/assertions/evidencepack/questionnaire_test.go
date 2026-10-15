package evidencepack

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/validate"
)

// exampleQuestionnaire is the generic starting point the template ships to every
// instance. The tests below hold its LLZ claims to the code they describe.
const exampleQuestionnaire = "instance-template/infosec/questionnaire.yaml.example"

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

func loadExample(t *testing.T) *Questionnaire {
	t.Helper()
	q, err := ParseQuestionnaire([]byte(readRepo(t, exampleQuestionnaire)), nil)
	if err != nil {
		t.Fatalf("the shipped example does not validate: %v", err)
	}
	return q
}

func row(t *testing.T, q *Questionnaire, n int) Requirement {
	t.Helper()
	for _, r := range q.Requirements {
		if r.Num == n {
			return r
		}
	}
	t.Fatalf("the example has no row %d", n)
	return Requirement{}
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

// ── schema ───────────────────────────────────────────────────────────────────

const validQ = `apiVersion: llz.akamai-consulting.io/v1alpha1
kind: EvidenceQuestionnaire
metadata: {name: my-org}
requirements:
  - {num: 1, title: Account, adopter: Ask.}
  - {num: 2, title: Rotation, llz: Measured., gates: [assert-rotation-health]}
`

func TestParseQuestionnaireAcceptsAValidFile(t *testing.T) {
	q, err := ParseQuestionnaire([]byte(validQ), map[string]bool{"assert-rotation-health": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Requirements) != 2 || strings.Join(q.CitedGates(), ",") != "assert-rotation-health" {
		t.Errorf("parsed %+v", q)
	}
}

// FAIL CLOSED: every malformed shape is refused, and the error names it.
func TestParseQuestionnaireRefusesMalformedFiles(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"unknown key (a typo drops an answer)", strings.Replace(validQ, "adopter: Ask.", "adoptor: Ask.", 1), "adoptor"},
		{"wrong apiVersion", strings.Replace(validQ, "v1alpha1", "v9", 1), "apiVersion"},
		{"wrong kind", strings.Replace(validQ, "EvidenceQuestionnaire", "Questionnaire", 1), "kind"},
		{"no name", strings.Replace(validQ, "{name: my-org}", "{}", 1), "metadata.name"},
		{"no rows", strings.SplitAfter(validQ, "requirements:\n")[0] + "  []\n", "requirements is empty"},
		{"duplicate row", strings.Replace(validQ, "num: 2", "num: 1", 1), "answered twice"},
		{"row zero", strings.Replace(validQ, "num: 2", "num: 0", 1), "positive"},
		{"no title", strings.Replace(validQ, "title: Account, ", "", 1), "no title"},
		{"nobody satisfies it", strings.Replace(validQ, ", adopter: Ask.", "", 1), "nobody satisfies"},
		{"gate with a prefix", strings.Replace(validQ, "[assert-rotation-health]", "[llz ci assert-rotation-health]", 1), "bare `llz ci` verb"},
		{"gate that is not a verb", strings.Replace(validQ, "[assert-rotation-health]", "[assert-nothing]", 1), "not a registered verb"},
		{"not yaml", "{", "questionnaire"},
	}
	known := map[string]bool{"assert-rotation-health": true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseQuestionnaire([]byte(tc.body), known)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadQuestionnaire(t *testing.T) {
	missing := func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if q, err := LoadQuestionnaire(missing, "infosec/questionnaire.yaml", nil); q != nil || err != nil {
		t.Errorf("absent file = (%v, %v), want (nil, nil): the pack records it as not provided", q, err)
	}
	denied := func(string) ([]byte, error) { return nil, errors.New("path escapes the repository tree") }
	if _, err := LoadQuestionnaire(denied, "/etc/passwd", nil); err == nil {
		t.Error("an unreadable questionnaire must fail, not read as absent")
	}
	bad := func(string) ([]byte, error) { return []byte("kind: nope\n"), nil }
	if _, err := LoadQuestionnaire(bad, "q.yaml", nil); err == nil || !strings.Contains(err.Error(), "q.yaml") {
		t.Errorf("an invalid questionnaire must fail naming the file, got %v", err)
	}
}

// ── the shipped example ──────────────────────────────────────────────────────

// The example is the template's starting point for every adopter, so it must stay
// org-neutral: no one org's intake process, vendor or naming.
func TestExampleIsOrgNeutral(t *testing.T) {
	body := strings.ToLower(readRepo(t, exampleQuestionnaire))
	for _, banned := range []string{"sog", "akamai-internal", "wiz", "practitioner", "company name", "eaa", "control center"} {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(banned) + `\b`).MatchString(body) {
			t.Errorf("the generic example names %q — org-specific answers belong in that org's own questionnaire file", banned)
		}
	}
}

// Every cited path exists. A rename that leaves an answer pointing at nothing is
// the stale-link class (llz-node-firewall, llz-pool, a retired cred-audit).
func TestExampleRefsExist(t *testing.T) {
	q := loadExample(t)
	n := 0
	for _, r := range q.Requirements {
		for _, ref := range r.Refs {
			n++
			if _, err := os.Stat(filepath.Join(repoRoot(t), filepath.FromSlash(ref))); err != nil {
				t.Errorf("#%d cites %s, which does not exist", r.Num, ref)
			}
		}
	}
	if n == 0 {
		t.Fatal("the example cites no paths — this test examined nothing")
	}
}

// EVERY CITED GATE IS RECORDED. A gate the pack cites but the scheduled job never
// passes renders "not run" on its row every day — a permanent hole that reads as
// evidence. The consumer is the delivered workflow's --gate flags.
func TestEveryGateTheExampleCitesIsRecordedByTheScheduledJob(t *testing.T) {
	q := loadExample(t)
	wf := readRepo(t, "instance-template/.github/workflows/llz-scheduled-checks.yml")
	recorded := map[string]bool{}
	for _, m := range regexp.MustCompile(`--gate\s+"([a-z0-9-]+)=`).FindAllStringSubmatch(wf, -1) {
		recorded[m[1]] = true
	}
	cited := q.CitedGates()
	if len(cited) == 0 {
		t.Fatal("the example cites no gates — this test examined nothing")
	}
	for _, g := range cited {
		if !recorded[g] {
			t.Errorf("the example cites `llz ci %s`, but llz-scheduled-checks.yml never passes --gate %s=…: its row would always read \"not run\"", g, g)
		}
	}
}

// SPLIT CONTRACT: the auditor-facing doc documents the example row by row — it
// must cite every path and gate the example cites, and state every gap.
func TestControlMappingDocCitesWhatTheExampleCites(t *testing.T) {
	q := loadExample(t)
	rows := tableRows(t, readRepo(t, "docs/infosec/control-mapping.md"), "## Section A")
	for _, r := range q.Requirements {
		docRow, ok := rows[r.Num]
		if !ok {
			t.Errorf("control-mapping.md Section A has no row %d (%s)", r.Num, r.Title)
			continue
		}
		for _, ref := range r.Refs {
			if !strings.Contains(docRow, ref) {
				t.Errorf("control-mapping.md row %d does not cite %s", r.Num, ref)
			}
		}
		for _, g := range r.Gates {
			if !strings.Contains(docRow, "`llz ci "+g+"`") {
				t.Errorf("control-mapping.md row %d does not name gate `llz ci %s`", r.Num, g)
			}
		}
		if r.Gap != "" && !strings.Contains(docRow, "Gap") {
			t.Errorf("control-mapping.md row %d omits the gap the example states: %s", r.Num, r.Gap)
		}
	}
	for n := range rows {
		found := false
		for _, r := range q.Requirements {
			found = found || r.Num == n
		}
		if !found {
			t.Errorf("control-mapping.md Section A row %d has no row in the example", n)
		}
	}
}

// ── the example's LLZ claims, held to the code ──────────────────────────────

// hclBlock returns the body of the first block in src whose header matches
// header (a regexp, e.g. `resource\s+"linode_firewall"\s+"this"`), by brace
// matching — so neither `tofu fmt` alignment nor attribute order matters.
func hclBlock(t *testing.T, src, header string) string {
	t.Helper()
	loc := regexp.MustCompile(header + `\s*\{`).FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no block matching %q", header)
	}
	depth := 0
	for i := loc[1] - 1; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[loc[1]:i]
			}
		}
	}
	t.Fatalf("block %q is not closed", header)
	return ""
}

// hclAttr returns the raw expression of a top-level-of-body attribute.
func hclAttr(body, name string) (string, bool) {
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*(.+?)\s*$`).FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func wantAttr(t *testing.T, claim int, file, block, body, name, want string) {
	t.Helper()
	got, ok := hclAttr(body, name)
	if !ok || got != want {
		t.Errorf("answer #%d claims %s %s sets %s = %s; it reads %q — fix the answer and the doc, or the code", claim, file, block, name, want, got)
	}
}

// fwRule is one inline node-firewall rule from firewall.tf's baseline list.
type fwRule struct{ label, protocol, ports, ipv4, ipv6 string }

func firewallRules(t *testing.T, src string) []fwRule {
	t.Helper()
	re := regexp.MustCompile(`\{\s*label\s*=\s*"([^"]+)"\s*,\s*protocol\s*=\s*"([^"]+)"\s*,\s*ports\s*=\s*(null|"[^"]*")\s*,\s*ipv4\s*=\s*(null|\[[^\]]*\])\s*,\s*ipv6\s*=\s*(null|\[[^\]]*\])\s*\}`)
	var out []fwRule
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out = append(out, fwRule{m[1], m[2], strings.Trim(m[3], `"`), m[4], m[5]})
	}
	if len(out) == 0 {
		t.Fatal("parsed no firewall rules from firewall.tf — the parser, not the claim, is broken")
	}
	return out
}

func portsCover(ports string, p int) bool {
	for _, part := range strings.Split(ports, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		if p >= a && p <= b {
			return true
		}
	}
	return false
}

// claimChecks pins every row of the example whose LLZ answer makes a claim about
// the code. A row that says "hardcoded" without an entry here fails
// TestEveryHardcodedClaimIsChecked.
var claimChecks = map[int]func(t *testing.T){
	2: func(t *testing.T) {
		mod := readRepo(t, "terraform-modules/llz-cluster/main.tf")
		wantAttr(t, 2, "llz-cluster/main.tf", "linode_lke_cluster", hclBlock(t, mod, `resource\s+"linode_lke_cluster"\s+"this"`), "tags", "var.tags")
		fw := readRepo(t, "terraform-modules/llz-cluster/firewall.tf")
		wantAttr(t, 2, "llz-cluster/firewall.tf", "linode_firewall", hclBlock(t, fw, `resource\s+"linode_firewall"\s+"this"`), "tags", "var.tags")
		root := readRepo(t, "tools/internal/shared/tfroots/roots/cluster/main.tf")
		wantAttr(t, 2, "roots/cluster/main.tf", "module cluster", hclBlock(t, root, `module\s+"cluster"`), "tags", "var.tags")
		pool, _ := hclAttr(hclBlock(t, root, `resource\s+"linode_lke_node_pool"\s+"this"`), "tags")
		if !regexp.MustCompile(`^concat\(\s*var\.tags\s*,`).MatchString(pool) {
			t.Errorf("answer #2 claims the node pool carries spec.cluster.tags; roots/cluster/main.tf node pool tags = %q", pool)
		}
	},
	3: func(t *testing.T) {
		src := readRepo(t, "terraform-modules/llz-cluster/firewall.tf")
		wantAttr(t, 3, "llz-cluster/firewall.tf", "linode_firewall", hclBlock(t, src, `resource\s+"linode_firewall"\s+"this"`), "inbound_policy", `"DROP"`)
		scoped := map[string]bool{"[var.vpc_subnet_cidr]": true, "[var.control_plane_cidr]": true, "[var.nodebalancer_cidr]": true}
		icmpOpen := false
		for _, r := range firewallRules(t, src) {
			if r.label == "allow-icmp" {
				icmpOpen = strings.Contains(r.ipv4, "0.0.0.0/0") || strings.Contains(r.ipv6, "::/0")
				continue
			}
			if !scoped[r.ipv4] || r.ipv6 != "null" {
				t.Errorf("answer #3 claims every inbound rule is scoped to the VPC / control-plane / NodeBalancer range; %s admits ipv4=%s ipv6=%s", r.label, r.ipv4, r.ipv6)
			}
		}
		// The gap is stated exactly while the rule ships, and must go when it does.
		var q3 Requirement
		for _, r := range loadExample(t).Requirements {
			if r.Num == 3 {
				q3 = r
			}
		}
		if icmpOpen && !strings.Contains(q3.Gap, "ICMP") {
			t.Error("#3 must state the open-world ICMP rule as a gap while the firewall ships it")
		}
		if !icmpOpen && strings.Contains(q3.Gap, "ICMP") {
			t.Error("the open-world ICMP rule is gone — remove it from #3's gap")
		}
	},
	6: func(t *testing.T) {
		for _, r := range firewallRules(t, readRepo(t, "terraform-modules/llz-cluster/firewall.tf")) {
			if portsCover(r.ports, 30000) && r.ipv4 != "[var.nodebalancer_cidr]" && r.ipv4 != "[var.vpc_subnet_cidr]" {
				t.Errorf("answer #6 claims NodePorts are exposed only to the NodeBalancer range; %s admits %s", r.label, r.ipv4)
			}
			if r.ipv4 != "[var.vpc_subnet_cidr]" && r.ipv4 != "[var.control_plane_cidr]" && r.label != "allow-icmp" && !portsCover(r.ports, 30000) {
				t.Errorf("answer #6 claims nothing beyond NodePorts is exposed outside the VPC / control plane; %s opens %s to %s", r.label, r.ports, r.ipv4)
			}
		}
	},
	7: func(t *testing.T) {
		for _, r := range firewallRules(t, readRepo(t, "terraform-modules/llz-cluster/firewall.tf")) {
			if r.protocol == "TCP" && portsCover(r.ports, 22) && (r.ipv4 != "[var.vpc_subnet_cidr]" || r.ipv6 != "null") {
				t.Errorf("answer #7 claims TCP/22 is admitted only from the VPC subnet; %s admits ipv4=%s ipv6=%s", r.label, r.ipv4, r.ipv6)
			}
		}
	},
	9: func(t *testing.T) {
		if !regexp.MustCompile(`(?m)^\s*linodebs\.csi\.linode\.com/encrypted:\s*"true"\s*$`).MatchString(readRepo(t, "tools/internal/extensions/lifecycle/bootstrapcluster/manifests/block-storage-class.yaml")) {
			t.Error("answer #9 claims the default StorageClass is encrypted; block-storage-class.yaml no longer sets linodebs.csi.linode.com/encrypted: \"true\"")
		}
	},
	10: func(t *testing.T) {
		mod := readRepo(t, "terraform-modules/llz-cluster/main.tf")
		acl := hclBlock(t, hclBlock(t, hclBlock(t, mod, `resource\s+"linode_lke_cluster"\s+"this"`), `control_plane`), `acl`)
		wantAttr(t, 10, "llz-cluster/main.tf", "control_plane.acl", acl, "enabled", "true")
		for _, open := range []string{"0.0.0.0/0", "::/0", "203.0.113.7/0"} {
			if !validate.IsOpenWorldCIDR(open) {
				t.Errorf("answer #10 claims open-world entries are refused; the validator accepts %s", open)
			}
		}
		if err := validate.CIDRList("acl", "0.0.0.0/0", validate.IPv4); err == nil {
			t.Error("answer #10 claims the CIDR validator refuses 0.0.0.0/0; CIDRList accepted it")
		}
	},
	11: func(t *testing.T) {
		mod := readRepo(t, "terraform-modules/llz-cluster/main.tf")
		wantAttr(t, 11, "llz-cluster/main.tf", "linode_lke_cluster", hclBlock(t, mod, `resource\s+"linode_lke_cluster"\s+"this"`), "tier", `"enterprise"`)
	},
	12: func(t *testing.T) {
		root := readRepo(t, "tools/internal/shared/tfroots/roots/cluster/main.tf")
		wantAttr(t, 12, "roots/cluster/main.tf", "linode_lke_node_pool", hclBlock(t, root, `resource\s+"linode_lke_node_pool"\s+"this"`), "disk_encryption", `"enabled"`)
	},
}

func TestExampleClaimsHold(t *testing.T) {
	q := loadExample(t)
	nums := make([]int, 0, len(claimChecks))
	for n := range claimChecks {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		if row(t, q, n).LLZ == "" {
			t.Errorf("claim check #%d has no LLZ answer to check — the row moved; re-key the check", n)
			continue
		}
		t.Run(strconv.Itoa(n), claimChecks[n])
	}
}

// A "hardcoded" answer is a promise to an approver that a value is not a
// variable. Every such row must be pinned above, or the promise is unchecked.
func TestEveryHardcodedClaimIsChecked(t *testing.T) {
	for _, r := range loadExample(t).Requirements {
		if strings.Contains(strings.ToLower(r.LLZ), "hardcoded") {
			if _, ok := claimChecks[r.Num]; !ok {
				t.Errorf("#%d (%s) claims something is hardcoded but no claim check pins it", r.Num, r.Title)
			}
		}
	}
}

// The checks are whitespace-robust: realign the source the way `tofu fmt` would
// and every claim still parses.
func TestClaimParsersSurviveReformatting(t *testing.T) {
	src := "resource \"linode_lke_cluster\" \"this\" {\n  label = \"x\"\n  tier = \"enterprise\"\n  control_plane {\n    acl {\n      enabled    =    true\n    }\n  }\n}\n"
	body := hclBlock(t, src, `resource\s+"linode_lke_cluster"\s+"this"`)
	if v, _ := hclAttr(body, "tier"); v != `"enterprise"` {
		t.Errorf("tier = %q", v)
	}
	if v, _ := hclAttr(hclBlock(t, hclBlock(t, body, `control_plane`), `acl`), "enabled"); v != "true" {
		t.Errorf("acl.enabled = %q", v)
	}
	rules := firewallRules(t, `{ label = "a",   protocol = "TCP", ports = "22",  ipv4 = [var.vpc_subnet_cidr],  ipv6 = null },`)
	if len(rules) != 1 || !portsCover(rules[0].ports, 22) || portsCover("10250,10256", 22) || !portsCover("1-65535", 22) {
		t.Errorf("rule parse = %+v", rules)
	}
}
