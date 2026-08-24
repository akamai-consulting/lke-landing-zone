package upgradeplan

import (
	"bytes"
	"strings"
	"testing"
)

// planJSON builds a `tofu show -json`-shaped document from (address, actions)
// pairs. Shaped like the real thing — format_version and all — so the parser is
// exercised against what tofu emits rather than against the struct it fills.
func planJSON(t *testing.T, entries ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"format_version":"1.2","terraform_version":"1.12.5","resource_changes":[`)
	for i := 0; i < len(entries); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"address":"` + entries[i] + `","type":"linode_lke_cluster",` +
			`"change":{"actions":[` + entries[i+1] + `]}}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// ── classify ─────────────────────────────────────────────────────────────────

// BOTH SPELLINGS OF A REPLACE. `["delete","create"]` is the ordinary one and
// `["create","delete"]` is the same replace under create_before_destroy. Matching
// the pair explicitly — rather than asking "is there a delete in here" — is how
// the second one passes silently, and create_before_destroy is exactly what a
// module author reaches for to make a replace less disruptive.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actions []string
		want    string
	}{
		{"no-op", []string{"no-op"}, ""},
		{"create", []string{"create"}, ""},
		{"update in place", []string{"update"}, ""},
		{"read", []string{"read"}, ""},
		{"destroy", []string{"delete"}, "destroy"},
		{"replace", []string{"delete", "create"}, "replace"},
		{"replace, create_before_destroy", []string{"create", "delete"}, "replace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.actions); got != tc.want {
				t.Errorf("classify(%v) = %q, want %q", tc.actions, got, tc.want)
			}
		})
	}
}

// ── Evaluate ─────────────────────────────────────────────────────────────────

// THE BEHAVIOR: an upgrade that would recycle a live cluster is a finding, and
// the finding names the address so a reader can go find the forcing attribute.
func TestEvaluateFlagsAReplacedCluster(t *testing.T) {
	p, err := Parse([]byte(planJSON(t,
		"module.cluster.linode_lke_cluster.this", `"delete","create"`,
		"module.cluster.linode_firewall.this", `"update"`,
	)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := Evaluate(p)
	if len(v.Destructive) != 1 {
		t.Fatalf("Destructive = %+v, want exactly the cluster", v.Destructive)
	}
	f := v.Destructive[0]
	if f.Address != "module.cluster.linode_lke_cluster.this" || f.Kind != "replace" {
		t.Errorf("finding = %+v, want the cluster as a replace", f)
	}
	if v.Total != 2 || v.Updates != 1 {
		t.Errorf("Total=%d Updates=%d; the counts are what make a pass evidence rather than a claim",
			v.Total, v.Updates)
	}
}

// An upgrade legitimately adds resources and changes attributes. Failing on those
// would make the gate red on every real upgrade, and a gate that is always red is
// one people route around.
func TestEvaluatePassesCreatesAndUpdates(t *testing.T) {
	p, err := Parse([]byte(planJSON(t,
		"module.cluster.linode_lke_cluster.this", `"update"`,
		"module.cluster.linode_object_storage_bucket.new", `"create"`,
		"module.cluster.linode_firewall.this", `"no-op"`,
	)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := Evaluate(p)
	if len(v.Destructive) != 0 {
		t.Errorf("Destructive = %+v; creates and in-place updates must pass", v.Destructive)
	}
	if v.Creates != 1 || v.Updates != 1 {
		t.Errorf("Creates=%d Updates=%d, want 1 and 1", v.Creates, v.Updates)
	}
}

// Findings are sorted, so two runs over the same plan produce the same message
// and a diff between them means the plan changed.
func TestEvaluateSortsFindings(t *testing.T) {
	p, _ := Parse([]byte(planJSON(t,
		"module.z.linode_volume.b", `"delete"`,
		"module.a.linode_volume.a", `"delete"`,
	)))
	v := Evaluate(p)
	if len(v.Destructive) != 2 || v.Destructive[0].Address != "module.a.linode_volume.a" {
		t.Errorf("findings not sorted by address: %+v", v.Destructive)
	}
}

// ── Parse: the fail-closed arms ──────────────────────────────────────────────

// A DOCUMENT THAT IS NOT A PLAN MUST NOT PRODUCE A VERDICT. `{}` unmarshals
// happily into a Plan with no changes, so without the format_version check the
// gate reports "0 destructive" — its strongest verdict — about a file it did not
// understand. That is the shape of every gate that goes quiet.
func TestParseRefusesADocumentThatIsNotAPlan(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty object", `{}`},
		{"some other json", `{"hello":"world"}`},
		{"the human-readable plan", "Terraform will perform the following actions:"},
		{"truncated", `{"format_version":"1.2","resource_changes":[`},
		// THE ONE THE DELIVERED PIPELINE CAN ACTUALLY PRODUCE. The workflow runs
		// `tofu show -json tfplan.bin | llz ci assert-upgrade-plan`; if tofu show
		// fails, llz is handed an empty stdin. Accepting that as "nothing to report"
		// would turn a broken plan step into a green gate.
		{"empty stdin, from a failed tofu show", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.body)); err == nil {
				t.Errorf("Parse(%q) returned no error; a document that is not a plan must not "+
					"produce a verdict", tc.body)
			}
		})
	}
}

// An empty resource_changes IS a legitimate verdict — an upgrade that proposes
// nothing — and the array being present is evidence the document was understood.
// This is the one "empty" that must be accepted, and Run says so out loud.
func TestParseAcceptsAPlanWithNoChanges(t *testing.T) {
	p, err := Parse([]byte(`{"format_version":"1.2","resource_changes":[]}`))
	if err != nil {
		t.Fatalf("a plan proposing nothing is valid: %v", err)
	}
	if v := Evaluate(p); v.Total != 0 || len(v.Destructive) != 0 {
		t.Errorf("Evaluate = %+v, want an empty verdict", v)
	}
}

// ── Run: the report ──────────────────────────────────────────────────────────

func TestRunFailsAndExplainsADestructivePlan(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run("-", false, &out, &errOut,
		strings.NewReader(planJSON(t, "module.cluster.linode_lke_cluster.this", `"delete","create"`)))
	if err == nil {
		t.Fatal("a plan that replaces a live cluster must fail the gate")
	}
	report := errOut.String()
	for _, want := range []string{
		"module.cluster.linode_lke_cluster.this", // which resource
		"forces replacement",                     // how to find the cause
		"release notes",                          // the escape hatch, if it is intended
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report never mentions %q — the reader cannot act on it:\n%s", want, report)
		}
	}
	// GitHub parses an annotation only at the start of a line.
	if !strings.HasPrefix(report, "::error::") && !strings.Contains(report, "\n::error::") {
		t.Errorf("no line-initial ::error annotation, so CI will not surface this:\n%s", report)
	}
}

// A PASS MUST SAY HOW MUCH IT EXAMINED. "no destructive changes" over 40
// resources and over 0 are the same words and very different claims — and the
// second is what a plan taken against the wrong state key looks like, which is a
// real way to get a green run that proves nothing.
func TestRunReportsWhatItExamined(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("-", false, &out, &errOut,
		strings.NewReader(planJSON(t, "module.cluster.linode_lke_cluster.this", `"update"`))); err != nil {
		t.Fatalf("Run: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "1 resource change(s) examined") {
		t.Errorf("the pass does not say what it examined:\n%s", out.String())
	}
}

// An all-empty plan passes, but it must say so distinctly rather than printing
// the same line as a plan it actually checked.
func TestRunCallsOutAPlanThatProposesNothing(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("-", false, &out, &errOut,
		strings.NewReader(`{"format_version":"1.2","resource_changes":[]}`)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "state key") {
		t.Errorf("an empty plan must point at the likeliest cause of an unexpected one:\n%s", out.String())
	}
}

func TestRunSurfacesAnUnreadablePlan(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("/nonexistent/plan.json", false, &out, &errOut, strings.NewReader("")); err == nil {
		t.Error("an unreadable plan must be an error, not an empty verdict")
	}
}

// ── --expect-no-changes ──────────────────────────────────────────────────────

// THE BEHAVIOR THE E2E LANE DEPENDS ON. A plan taken straight after an apply must
// be empty; a resource still proposing an update is one Terraform cannot bring to
// rest, and it will churn every future apply.
func TestExpectNoChangesFlagsAPerpetualUpdate(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run("-", true, &out, &errOut,
		strings.NewReader(planJSON(t,
			"module.cluster.linode_lke_cluster.this", `"update"`,
			"module.cluster.linode_firewall.this", `"no-op"`,
		)))
	if err == nil {
		t.Fatal("a non-empty plan taken after an apply must fail --expect-no-changes")
	}
	report := errOut.String()
	if !strings.Contains(report, "module.cluster.linode_lke_cluster.this") {
		t.Errorf("report does not name the resource that will not settle:\n%s", report)
	}
	// The vpc_id shape is the reason this mode exists — the message has to point at
	// it, or a reader sees "update in place" and assumes it is benign.
	if !strings.Contains(report, "vpc_id") || !strings.Contains(report, "ignore_changes") {
		t.Errorf("report does not explain the create-time-only class or its remedy:\n%s", report)
	}
	// The firewall is a no-op and must NOT be reported: listing every resource
	// Terraform read would bury the one that actually differs.
	if strings.Contains(report, "linode_firewall") {
		t.Errorf("a no-op entry was reported as a change:\n%s", report)
	}
}

// An empty plan is the pass, and it must stay a pass under the strict flag.
func TestExpectNoChangesPassesAnEmptyPlan(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("-", true, &out, &errOut,
		strings.NewReader(`{"format_version":"1.2","resource_changes":[]}`)); err != nil {
		t.Fatalf("an empty plan must pass --expect-no-changes: %v\n%s", err, errOut.String())
	}
}

// A plan of nothing but no-ops is also empty in every sense that matters.
// Terraform lists every resource it read, so counting those would make a settled
// cluster look like a busy one and the gate would be red on every correct run.
func TestExpectNoChangesIgnoresNoOpAndReadEntries(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("-", true, &out, &errOut,
		strings.NewReader(planJSON(t,
			"module.cluster.linode_lke_cluster.this", `"no-op"`,
			"data.linode_instances.nodes", `"read"`,
		))); err != nil {
		t.Fatalf("no-op and read entries are not changes: %v\n%s", err, errOut.String())
	}
}

// THE TWO VERDICTS MUST NOT COMPETE. A plan that destroys something is reported
// as destroying something — not as "unexpected changes" — because the two have
// completely different remedies and the destructive one is strictly more urgent.
func TestExpectNoChangesDefersToTheDestructiveVerdict(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run("-", true, &out, &errOut,
		strings.NewReader(planJSON(t, "module.cluster.linode_lke_cluster.this", `"delete","create"`)))
	if err == nil {
		t.Fatal("a destructive plan must still fail")
	}
	if !strings.Contains(err.Error(), "destroyed or replaced") {
		t.Errorf("a destroy was reported as an unexpected change: %v", err)
	}
	if strings.Contains(errOut.String(), "straight after an apply") {
		t.Errorf("both verdicts fired at once; the destructive one must win:\n%s", errOut.String())
	}
}

// Without the flag, an ordinary update passes — the default mode is comparing
// releases, where creates and updates are exactly what an upgrade is made of.
func TestUpdatesPassWithoutTheStrictFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("-", false, &out, &errOut,
		strings.NewReader(planJSON(t, "module.cluster.linode_lke_cluster.this", `"update"`))); err != nil {
		t.Fatalf("an in-place update must pass the default mode: %v", err)
	}
}

// ── The bucket-rename remedy ──────────────────────────────────────────────────
//
// The fixture is gsap-apl's real prod plan under v0.0.45: `2 to add, 0 to
// change, 2 to destroy`, both destroys a bucket whose label prefix moved from
// the module default `platform` to the per-instance `gsap-apl`.

const objRenamePlan = `{"format_version":"1.2","resource_changes":[
 {"address":"module.object_storage.linode_object_storage_bucket.loki_chunks",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["delete","create"],
            "before":{"label":"platform-loki-chunks-prod"},
            "after":{"label":"gsap-apl-loki-chunks-prod"}}},
 {"address":"module.object_storage.linode_object_storage_bucket.harbor_registry",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["delete","create"],
            "before":{"label":"platform-harbor-registry-prod"},
            "after":{"label":"gsap-apl-harbor-registry-prod"}}}]}`

func TestRenameRemedyNamesThePrefixToPin(t *testing.T) {
	p, err := Parse([]byte(objRenamePlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := Evaluate(p)
	if len(v.Destructive) != 2 {
		t.Fatalf("destructive = %d, want 2", len(v.Destructive))
	}
	got := RenameRemedy(v.Destructive)
	if got == "" {
		t.Fatal("a bucket rename must produce a remedy")
	}
	// THE ONE INSTRUCTION THAT WORKS. Without the prefix named, the operator is
	// left with the generic "add a moved{} block" advice, and there is no moved{}
	// for a bucket label.
	if !strings.Contains(got, "objLabelPrefix: platform") {
		t.Errorf("remedy must name the prefix to pin; got:\n%s", got)
	}
	if !strings.Contains(got, "platform-loki-chunks-prod -> gsap-apl-loki-chunks-prod") {
		t.Errorf("remedy must show the rename it diagnosed; got:\n%s", got)
	}
	// The silent case is the dangerous one and the text must say so.
	if !strings.Contains(got, "empty bucket would have been deleted") {
		t.Errorf("remedy must explain why this failed loudly rather than silently; got:\n%s", got)
	}
}

// A destroy that is NOT a rename gets the generic advice, not a prefix claim.
func TestRenameRemedySilentOnANonRename(t *testing.T) {
	cases := map[string][]Finding{
		"not a bucket": {{Address: "module.cluster.linode_lke_cluster.this", Kind: "replace",
			Type: "linode_lke_cluster", BeforeLabel: "a", AfterLabel: "b"}},
		"bucket destroyed, not renamed": {{Address: "m.linode_object_storage_bucket.x", Kind: "destroy",
			Type: objBucketType, BeforeLabel: "acme-loki-chunks-prod"}},
		"same label": {{Address: "m.linode_object_storage_bucket.x", Kind: "replace",
			Type: objBucketType, BeforeLabel: "acme-loki-chunks-prod", AfterLabel: "acme-loki-chunks-prod"}},
		"no findings": nil,
	}
	for name, findings := range cases {
		if got := RenameRemedy(findings); got != "" {
			t.Errorf("%s: want no remedy, got:\n%s", name, got)
		}
	}
}

// TWO DIFFERENT PREFIX MOVES IN ONE PLAN is not a prefix change, so no prefix is
// claimed — but the renames are still reported. Guessing one of them would send
// an operator to pin a value that fixes half their buckets.
func TestRenameRemedyWithoutAgreementClaimsNoPrefix(t *testing.T) {
	got := RenameRemedy([]Finding{
		{Address: "a", Kind: "replace", Type: objBucketType, BeforeLabel: "platform-loki-chunks-prod", AfterLabel: "gsap-apl-loki-chunks-prod"},
		{Address: "b", Kind: "replace", Type: objBucketType, BeforeLabel: "other-harbor-registry-prod", AfterLabel: "different-harbor-registry-prod"},
	})
	if got == "" {
		t.Fatal("the renames must still be reported")
	}
	if strings.Contains(got, "objLabelPrefix:") {
		t.Errorf("no single prefix agrees, so none may be named; got:\n%s", got)
	}
}

func TestSplitPrefix(t *testing.T) {
	cases := []struct{ before, after, wantOld, wantNew string }{
		{"platform-loki-chunks-prod", "gsap-apl-loki-chunks-prod", "platform", "gsap-apl"},
		{"platform-harbor-registry-prod", "gsap-apl-harbor-registry-prod", "platform", "gsap-apl"},
		// A suffix that is added rather than a prefix that changes: the head of
		// the shorter label empties, and an empty prefix claim is suppressed by
		// RenameRemedy rather than printed as `objLabelPrefix: `.
		{"loki-chunks-prod", "acme-loki-chunks-prod", "", "acme"},
	}
	for _, c := range cases {
		gotOld, gotNew := splitPrefix(c.before, c.after)
		if gotOld != c.wantOld || gotNew != c.wantNew {
			t.Errorf("splitPrefix(%q,%q) = (%q,%q), want (%q,%q)",
				c.before, c.after, gotOld, gotNew, c.wantOld, c.wantNew)
		}
	}
}
