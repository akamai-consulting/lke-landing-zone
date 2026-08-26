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
	err := Run("-", false, nil, &out, &errOut,
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
	if err := Run("-", false, nil, &out, &errOut,
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
	if err := Run("-", false, nil, &out, &errOut,
		strings.NewReader(`{"format_version":"1.2","resource_changes":[]}`)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "state key") {
		t.Errorf("an empty plan must point at the likeliest cause of an unexpected one:\n%s", out.String())
	}
}

func TestRunSurfacesAnUnreadablePlan(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("/nonexistent/plan.json", false, nil, &out, &errOut, strings.NewReader("")); err == nil {
		t.Error("an unreadable plan must be an error, not an empty verdict")
	}
}

// ── --expect-no-changes ──────────────────────────────────────────────────────

// THE BEHAVIOR THE E2E LANE DEPENDS ON. A plan taken straight after an apply must
// be empty; a resource still proposing an update is one Terraform cannot bring to
// rest, and it will churn every future apply.
func TestExpectNoChangesFlagsAPerpetualUpdate(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run("-", true, nil, &out, &errOut,
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
	if err := Run("-", true, nil, &out, &errOut,
		strings.NewReader(`{"format_version":"1.2","resource_changes":[]}`)); err != nil {
		t.Fatalf("an empty plan must pass --expect-no-changes: %v\n%s", err, errOut.String())
	}
}

// A plan of nothing but no-ops is also empty in every sense that matters.
// Terraform lists every resource it read, so counting those would make a settled
// cluster look like a busy one and the gate would be red on every correct run.
func TestExpectNoChangesIgnoresNoOpAndReadEntries(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("-", true, nil, &out, &errOut,
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
	err := Run("-", true, nil, &out, &errOut,
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
	if err := Run("-", false, nil, &out, &errOut,
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
	got := RenameRemedy(v, nil, nil)
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
		if got := RenameRemedy(Verdict{Destructive: findings}, nil, nil); got != "" {
			t.Errorf("%s: want no remedy, got:\n%s", name, got)
		}
	}
}

// TWO DIFFERENT PREFIX MOVES IN ONE PLAN is not a prefix change, so no prefix is
// claimed — but the renames are still reported. Guessing one of them would send
// an operator to pin a value that fixes half their buckets.
func TestRenameRemedyWithoutAgreementClaimsNoPrefix(t *testing.T) {
	got := RenameRemedy(Verdict{Destructive: []Finding{
		{Address: "a", Kind: "replace", Type: objBucketType, BeforeLabel: "platform-loki-chunks-prod", AfterLabel: "gsap-apl-loki-chunks-prod"},
		{Address: "b", Kind: "replace", Type: objBucketType, BeforeLabel: "other-harbor-registry-prod", AfterLabel: "different-harbor-registry-prod"},
	}}, nil, nil)
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

// halfMigratedPlan is gsap-apl's real prod object-storage plan, reduced to the
// four buckets and the two attributes this gate reads.
//
// The shape is the one that matters and it is not hypothetical: two buckets still
// under the OLD prefix are being renamed, and two are ALREADY under the new one
// and appear as `no-op` — which is how Terraform reports a resource it read and
// will not touch, carrying the same label in before and after.
const halfMigratedPlan = `{"format_version":"1.2","resource_changes":[
 {"address":"module.object_storage.linode_object_storage_bucket.harbor_registry",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["delete","create"],
            "before":{"label":"platform-harbor-registry-prod"},
            "after":{"label":"gsap-apl-harbor-registry-prod"}}},
 {"address":"module.object_storage.linode_object_storage_bucket.loki_chunks",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["delete","create"],
            "before":{"label":"platform-loki-chunks-prod"},
            "after":{"label":"gsap-apl-loki-chunks-prod"}}},
 {"address":"module.object_storage.linode_object_storage_bucket.loki_admin",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["no-op"],
            "before":{"label":"gsap-apl-loki-admin-prod"},
            "after":{"label":"gsap-apl-loki-admin-prod"}}},
 {"address":"module.object_storage.linode_object_storage_bucket.loki_ruler",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["no-op"],
            "before":{"label":"gsap-apl-loki-ruler-prod"},
            "after":{"label":"gsap-apl-loki-ruler-prod"}}}]}`

// TestHalfMigratedInstanceIsNotGivenAPrefixToPin is the gate on the remedy being
// SAFE rather than merely present.
//
// TestRenameRemedyNamesThePrefixToPin proves the advice appears. This proves it is
// withheld where following it would destroy something — the failure mode a remedy
// has that a diagnostic does not, and the one nothing here could see: every
// existing case reasons only about the buckets being renamed, and the evidence
// that disqualifies the claim is in the buckets that are NOT.
//
// Following the old advice on this plan produces `2 to add, 0 to change, 2 to
// destroy` a second time, on loki_admin and loki_ruler.
func TestHalfMigratedInstanceIsNotGivenAPrefixToPin(t *testing.T) {
	p, err := Parse([]byte(halfMigratedPlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := Evaluate(p)
	if len(v.Destructive) != 2 {
		t.Fatalf("destructive = %d, want the 2 renamed buckets", len(v.Destructive))
	}
	// Fail closed on the evidence itself: if the settled buckets stop being
	// collected, the check below cannot fire and would pass by examining nothing.
	if len(v.SettledBuckets) != 2 {
		t.Fatalf("settled buckets = %v, want the 2 the plan leaves alone — without them "+
			"the prefix claim is unguarded", v.SettledBuckets)
	}

	got := RenameRemedy(v, nil, nil)
	if got == "" {
		t.Fatal("the renames must still be reported — the operator still has to act")
	}
	// THE ASSERTION. `objLabelPrefix: platform` fits the two renames perfectly and
	// is exactly what this used to print.
	if strings.Contains(got, "objLabelPrefix: platform") {
		t.Errorf("remedy told the operator to pin `platform`, which renames the two buckets "+
			"already on `gsap-apl` and proposes destroying them instead; got:\n%s", got)
	}
	// Naming the stranded buckets is what makes the refusal actionable rather than
	// just a withheld answer.
	for _, want := range []string{"gsap-apl-loki-admin-prod", "gsap-apl-loki-ruler-prod", "half-migrated"} {
		if !strings.Contains(got, want) {
			t.Errorf("remedy must name %q so the operator can see why no prefix works; got:\n%s", want, got)
		}
	}
	// The renames themselves are still the finding.
	if !strings.Contains(got, "platform-loki-chunks-prod -> gsap-apl-loki-chunks-prod") {
		t.Errorf("remedy must still show the renames it diagnosed; got:\n%s", got)
	}
}

// ── The settled-bucket arms ──────────────────────────────────────────────────
//
// EVERY ONE OF THESE GOES THROUGH Parse + Evaluate rather than building a Verdict
// literal, and that is the point rather than a style choice. The first cut of
// these tests handed RenameRemedy a hand-written SettledBuckets, which meant they
// asserted the DECISION and could not see what Evaluate actually collects — and a
// collection bug (creates counted as settled) sat under a green suite that
// included a test written specifically to catch a suppressed-correct-remedy.

// renamePlan renders a plan JSON: renames from -> to, plus extra verbatim entries.
func renamePlan(from, to string, extra ...string) string {
	entries := []string{`
 {"address":"m.linode_object_storage_bucket.loki_chunks",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["delete","create"],
            "before":{"label":"` + from + `-loki-chunks-prod"},
            "after":{"label":"` + to + `-loki-chunks-prod"}}}`}
	entries = append(entries, extra...)
	return `{"format_version":"1.2","resource_changes":[` + strings.Join(entries, ",") + `]}`
}

func remedyFor(t *testing.T, planJSON string) string {
	t.Helper()
	p, err := Parse([]byte(planJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return RenameRemedy(Evaluate(p), nil, nil)
}

// The check keys on WOULD-THIS-BE-RENAMED, not on "is anything settled". A
// settled bucket already sitting under the prefix being recommended is untouched
// by that pin, so it is not evidence against it — and treating it as evidence
// would withhold a correct remedy from any instance that carries an extra bucket.
func TestSettledBucketsAlreadyUnderTheRecommendedPrefixDoNotSuppressIt(t *testing.T) {
	got := remedyFor(t, renamePlan("platform", "acme", `
 {"address":"m.linode_object_storage_bucket.loki_ruler",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["no-op"],
            "before":{"label":"platform-loki-ruler-prod"},
            "after":{"label":"platform-loki-ruler-prod"}}}`))
	if !strings.Contains(got, "objLabelPrefix: platform") {
		t.Errorf("a settled bucket already under the recommended prefix is left alone by that pin "+
			"and must not suppress the remedy; got:\n%s", got)
	}
}

// A bucket being CREATED is not evidence of a half-migration: it does not exist
// yet, so pinning the old prefix simply creates it there. Counting it as settled
// suppressed a correct remedy for any release that ADDS a bucket — and printed
// that the plan "leaves alone" a bucket it was creating.
func TestACreatedBucketIsNotEvidenceOfAHalfMigration(t *testing.T) {
	got := remedyFor(t, renamePlan("platform", "acme", `
 {"address":"m.linode_object_storage_bucket.loki_ruler",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["create"],
            "before":null,
            "after":{"label":"acme-loki-ruler-prod"}}}`))
	if !strings.Contains(got, "objLabelPrefix: platform") {
		t.Errorf("a bucket this plan CREATES suppressed a correct remedy; got:\n%s", got)
	}
	if strings.Contains(got, "acme-loki-ruler-prod") {
		t.Errorf("a bucket being created was reported as one the plan leaves alone; got:\n%s", got)
	}
}

// The separator is load-bearing. Without it, prefix "acme" would read
// "acme2-loki-chunks-prod" as already migrated, and the remedy would be offered on
// evidence that does not exist.
func TestSettledBucketsAreMatchedOnAWholePrefixComponent(t *testing.T) {
	got := remedyFor(t, renamePlan("acme", "beta", `
 {"address":"m.linode_object_storage_bucket.loki_ruler",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["no-op"],
            "before":{"label":"acme2-loki-ruler-prod"},
            "after":{"label":"acme2-loki-ruler-prod"}}}`))
	if strings.Contains(got, "objLabelPrefix: acme") {
		t.Errorf("`acme2-...` is not under `acme`, so pinning `acme` would rename it — "+
			"the remedy must be withheld; got:\n%s", got)
	}
}

// TestDisagreementIsStickyAcrossThreeOrMoreRenames is the arm
// TestRenameRemedyWithoutAgreementClaimsNoPrefix could not reach.
//
// That one uses TWO findings, and with two the disagreement is always detected on
// the last iteration, so nothing runs after the reset. Clearing was/now put the
// loop back into its "nothing seen yet" state, so with THREE renames whose odd one
// out is not last, the third re-seeded from itself and the disagreement was
// forgotten — printing a confident prefix plus "the plan goes empty and no bucket
// is touched" for a plan where pinning it destroys the non-conforming bucket.
//
// An off-by-one in a loop that produces DESTRUCTIVE ADVICE, invisible at the
// smallest input size that exercises the branch.
func TestDisagreementIsStickyAcrossThreeOrMoreRenames(t *testing.T) {
	rename := func(addr, before, after string) string {
		return `
 {"address":"m.linode_object_storage_bucket.` + addr + `",
  "type":"linode_object_storage_bucket",
  "change":{"actions":["delete","create"],
            "before":{"label":"` + before + `"},
            "after":{"label":"` + after + `"}}}`
	}
	// The odd one out is in the MIDDLE, so a later agreeing rename follows it.
	plan := `{"format_version":"1.2","resource_changes":[` + strings.Join([]string{
		rename("loki_chunks", "platform-loki-chunks-prod", "gsap-loki-chunks-prod"),
		rename("harbor_registry", "other-harbor-registry-prod", "different-harbor-registry-prod"),
		rename("loki_ruler", "platform-loki-ruler-prod", "gsap-loki-ruler-prod"),
	}, ",") + `]}`

	got := remedyFor(t, plan)
	if got == "" {
		t.Fatal("the renames must still be reported")
	}
	if strings.Contains(got, "objLabelPrefix:") {
		t.Errorf("a rename disagreed, so no prefix may be claimed — pinning one would destroy the "+
			"bucket that does not conform; got:\n%s", got)
	}
	// All three are still the finding, whatever the remedy decides.
	for _, want := range []string{"loki_chunks", "harbor_registry", "loki_ruler"} {
		if !strings.Contains(got, want) {
			t.Errorf("remedy must still report %s; got:\n%s", want, got)
		}
	}
}

// TestAPlanWithNoAgreedPrefixStillGetsAnInstruction.
//
// run.go returns as soon as RenameRemedy is non-empty, so the generic
// "WHAT THIS MEANS / WHAT TO DO" block never prints once a bucket rename is in
// the plan. That was fine while every rename plan got specific advice — and it
// stopped being fine when disagreement became sticky, which made "no prefix
// agreed" reachable for any plan whose renames do not all move the same way. The
// operator got the rename list and no instruction of any kind: the worst output a
// blocking gate can produce, because it names a problem and no next step.
func TestAPlanWithNoAgreedPrefixStillGetsAnInstruction(t *testing.T) {
	got := RenameRemedy(Verdict{Destructive: []Finding{
		{Address: "a", Kind: "replace", Type: objBucketType, BeforeLabel: "platform-loki-chunks-prod", AfterLabel: "gsap-loki-chunks-prod"},
		{Address: "b", Kind: "replace", Type: objBucketType, BeforeLabel: "other-harbor-registry-prod", AfterLabel: "different-harbor-registry-prod"},
	}}, nil, nil)
	if strings.Contains(got, "objLabelPrefix:") {
		t.Fatal("precondition: these renames disagree, so no prefix may be claimed")
	}
	if !strings.Contains(got, "WHAT TO DO") {
		t.Errorf("a plan this gate BLOCKS must always end with a next step; got:\n%s", got)
	}
	// The destroy path is where the confirmation for removing a bucket lives, and
	// an operator told only "decide per bucket" will reach for the apply again.
	if !strings.Contains(got, "destroy path") {
		t.Errorf("the instruction must name where a deliberate bucket removal belongs; got:\n%s", got)
	}
}

// Whatever RenameRemedy decides, it must never end without one — it is the only
// thing printed for a rename plan, and run.go returns straight after it.
func TestEveryRenameRemedyEndsWithAnInstruction(t *testing.T) {
	bucket := func(before, after string) Finding {
		return Finding{Address: "m." + before, Kind: "replace", Type: objBucketType, BeforeLabel: before, AfterLabel: after}
	}
	cases := map[string]Verdict{
		"agreed prefix": {Destructive: []Finding{bucket("platform-loki-chunks-prod", "acme-loki-chunks-prod")}},
		"disagreeing prefixes": {Destructive: []Finding{
			bucket("platform-loki-chunks-prod", "acme-loki-chunks-prod"),
			bucket("other-harbor-registry-prod", "different-harbor-registry-prod"),
		}},
		"half-migrated": {
			Destructive:    []Finding{bucket("platform-loki-chunks-prod", "acme-loki-chunks-prod")},
			SettledBuckets: []string{"acme-loki-ruler-prod"},
		},
	}
	for name, v := range cases {
		got := RenameRemedy(v, nil, nil)
		if got == "" {
			t.Errorf("%s: produced no remedy at all", name)
			continue
		}
		if !strings.Contains(got, "WHAT TO DO") {
			t.Errorf("%s: remedy ends without an instruction; got:\n%s", name, got)
		}
	}
}

// ── The census ───────────────────────────────────────────────────────────────

// TestAnEmptyBucketReplaceIsExemptAndADataBucketIsNot is the gate on the
// exemption that makes a prefix migration performable at all.
//
// Refusing every bucket replace is safe and also makes the CORRECT move
// impossible: an operator who pins the prefix that keeps their data-bearing
// buckets is then blocked by the empty ones that pin moves the other way. The
// exemption has to be exactly as wide as the evidence — a bucket the census says
// is empty — and no wider.
func TestAnEmptyBucketReplaceIsExemptAndADataBucketIsNot(t *testing.T) {
	bucket := func(before, after string) Finding {
		return Finding{Address: "m." + before, Kind: "replace", Type: objBucketType,
			BeforeLabel: before, AfterLabel: after, Actions: []string{"delete", "create"}}
	}
	v := Verdict{Destructive: []Finding{
		bucket("acme-loki-ruler-prod", "platform-loki-ruler-prod"),   // empty
		bucket("acme-loki-chunks-prod", "platform-loki-chunks-prod"), // 63k objects
	}}
	census := BucketCensus{"acme-loki-ruler-prod": 0, "acme-loki-chunks-prod": 63345}

	blocking, harmless := v.partition(census)
	if len(harmless) != 1 || harmless[0].BeforeLabel != "acme-loki-ruler-prod" {
		t.Errorf("the empty bucket must be exempt, got harmless=%v", harmless)
	}
	if len(blocking) != 1 || blocking[0].BeforeLabel != "acme-loki-chunks-prod" {
		t.Errorf("a bucket holding 63,345 objects must still block, got blocking=%v", blocking)
	}
}

// The exemption is granted on EVIDENCE, so its absence must never grant it. Every
// way the lookup can come back short lands here: no token, a failed request, an
// account whose buckets this token cannot see.
func TestAnUnknownBucketIsTreatedAsHoldingData(t *testing.T) {
	v := Verdict{Destructive: []Finding{{
		Address: "m.x", Kind: "replace", Type: objBucketType,
		BeforeLabel: "acme-loki-ruler-prod", AfterLabel: "platform-loki-ruler-prod",
	}}}
	for name, census := range map[string]BucketCensus{
		"nil census (no token, or the request failed)": nil,
		"empty census (account has no buckets)":        {},
		"census that knows other buckets only":         {"someone-elses-bucket": 0},
	} {
		blocking, harmless := v.partition(census)
		if len(harmless) != 0 || len(blocking) != 1 {
			t.Errorf("%s: an unverified bucket must block — the strict answer is the safe one; "+
				"blocking=%v harmless=%v", name, blocking, harmless)
		}
	}
}

// The exemption is for a REPLACE, which recreates the bucket. A bare destroy
// removes it and puts nothing back, so emptiness is not the only question and this
// gate is not the place to answer the rest.
func TestABareDestroyIsNeverExemptEvenWhenEmpty(t *testing.T) {
	v := Verdict{Destructive: []Finding{{
		Address: "m.x", Kind: "destroy", Type: objBucketType, BeforeLabel: "acme-loki-ruler-prod",
	}}}
	blocking, harmless := v.partition(BucketCensus{"acme-loki-ruler-prod": 0})
	if len(harmless) != 0 || len(blocking) != 1 {
		t.Errorf("a bucket being destroyed outright must block whatever it holds; blocking=%v harmless=%v",
			blocking, harmless)
	}
}

// A non-bucket resource has no census entry and must never be exempt — the
// worst version of this class is a linode_lke_cluster replace.
func TestANonBucketIsNeverExempt(t *testing.T) {
	v := Verdict{Destructive: []Finding{{
		Address: "module.cluster.linode_lke_cluster.this", Kind: "replace",
		Type: "linode_lke_cluster", BeforeLabel: "acme-prod", AfterLabel: "platform-prod",
	}}}
	blocking, harmless := v.partition(BucketCensus{"acme-prod": 0})
	if len(harmless) != 0 || len(blocking) != 1 {
		t.Errorf("only Object Storage buckets may be exempt; blocking=%v harmless=%v", blocking, harmless)
	}
}

// TestTheRemedyRecommendsThePrefixWhoseRenamesAreEmpty is the half that turns a
// blocked apply into a followable instruction.
//
// This is gsap-apl's real prod shape and its real object counts: two buckets under
// `platform` holding 63,345 and 46 objects, two under `gsap-apl` holding nothing.
// Pinning `platform` moves only the empty pair, so it is not a judgement call —
// it is the only move that loses nothing, and the census is what makes that
// sayable.
func TestTheRemedyRecommendsThePrefixWhoseRenamesAreEmpty(t *testing.T) {
	p, err := Parse([]byte(halfMigratedPlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	census := BucketCensus{
		"platform-loki-chunks-prod":     63345,
		"platform-harbor-registry-prod": 46,
		"gsap-apl-loki-admin-prod":      0,
		"gsap-apl-loki-ruler-prod":      0,
	}
	got := RenameRemedy(Evaluate(p), census, nil)

	if !strings.Contains(got, "objLabelPrefix: platform") {
		t.Errorf("with counts in hand there is one safe prefix and it must be named; got:\n%s", got)
	}
	// The evidence, not just the verdict: an operator about to move production
	// buckets should see what each holds.
	for _, want := range []string{"63345 objects", "46 objects", "EMPTY"} {
		if !strings.Contains(got, want) {
			t.Errorf("the recommendation must show %q as its evidence; got:\n%s", want, got)
		}
	}
}

// Without the census the recommendation must NOT appear — the same plan, and the
// honest answer is the one this printed before counts were available.
func TestWithoutACensusTheRemedyStillRefusesToPickAPrefix(t *testing.T) {
	p, err := Parse([]byte(halfMigratedPlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := RenameRemedy(Evaluate(p), nil, nil)
	if strings.Contains(got, "objLabelPrefix: platform") {
		t.Errorf("with no evidence about what the buckets hold, no prefix may be recommended; got:\n%s", got)
	}
	if !strings.Contains(got, "WHAT TO DO") {
		t.Errorf("it must still end with an instruction; got:\n%s", got)
	}
}

// TestTheRecommendationChecksTheKeyLabelsToo.
//
// The prefix names the Object Storage KEY labels as well as the bucket labels, and
// `llz reap` plus the credential-rotation table match key labels exactly. A
// recommendation checked against the buckets alone can therefore be right about
// the data and wrong about rotation — moving one problem into another that nobody
// is watching for. This used to be a caveat telling the operator to go and look;
// the same call that counts the objects lists the keys.
func TestTheRecommendationChecksTheKeyLabelsToo(t *testing.T) {
	p, err := Parse([]byte(halfMigratedPlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v := Evaluate(p)
	census := BucketCensus{
		"platform-loki-chunks-prod": 63345, "platform-harbor-registry-prod": 46,
		"gsap-apl-loki-admin-prod": 0, "gsap-apl-loki-ruler-prod": 0,
	}

	t.Run("says so when the keys already agree with the recommended prefix", func(t *testing.T) {
		// gsap-apl's real key labels.
		got := RenameRemedy(v, census, []string{"platform-loki-prod", "platform-harbor-registry-prod", "platform-obj-prod"})
		if !strings.Contains(got, "already\nagree") {
			t.Errorf("agreement is evidence FOR the recommendation and must be shown; got:\n%s", got)
		}
		if !strings.Contains(got, "platform-loki-prod") {
			t.Errorf("the agreeing key labels must be named; got:\n%s", got)
		}
	})

	t.Run("warns when NO key is under the prefix it is recommending", func(t *testing.T) {
		got := RenameRemedy(v, census, []string{"gsap-apl-loki-prod", "gsap-apl-obj-prod"})
		if !strings.Contains(got, "HEADS UP") {
			t.Errorf("recommending a prefix no key uses moves the problem into rotation, and must "+
				"be flagged; got:\n%s", got)
		}
	})

	t.Run("says it could not check rather than implying agreement", func(t *testing.T) {
		got := RenameRemedy(v, census, nil)
		if !strings.Contains(got, "could not list them") {
			t.Errorf("with no key evidence the remedy must say so, not stay silent; got:\n%s", got)
		}
	})
}

// ── The per-lane allowlist ────────────────────────────────────────────────────

// A node-pool resize is an ordinary operator action taken through the spec, and
// `linode_lke_node_pool.type` is ForceNew — so the cluster lane has to let it
// through while still refusing everything else in the same plan.
const poolResizeAndClusterRecyclePlan = `{"format_version":"1.2","resource_changes":[
 {"address":"linode_lke_node_pool.this","type":"linode_lke_node_pool",
  "change":{"actions":["delete","create"]}},
 {"address":"module.cluster.linode_lke_cluster.this","type":"linode_lke_cluster",
  "change":{"actions":["delete","create"]}}]}`

func TestPartitionAllowedLetsTheNodePoolThroughAndRefusesTheCluster(t *testing.T) {
	p, err := Parse([]byte(poolResizeAndClusterRecyclePlan))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	refused, allowed := PartitionAllowed(Evaluate(p).Destructive, []string{"linode_lke_node_pool"})
	if len(allowed) != 1 || allowed[0].Type != "linode_lke_node_pool" {
		t.Errorf("allowed = %+v, want just the node pool", allowed)
	}
	if len(refused) != 1 || refused[0].Type != "linode_lke_cluster" {
		t.Fatalf("refused = %+v, want just the LKE cluster", refused)
	}
}

// AN EMPTY ALLOWLIST REFUSES EVERYTHING — what object-storage, databases and the
// shared VPC pass. If this ever inverted, three lanes would silently stop
// guarding anything.
func TestPartitionAllowedWithNoAllowlistRefusesEverything(t *testing.T) {
	p, _ := Parse([]byte(poolResizeAndClusterRecyclePlan))
	refused, allowed := PartitionAllowed(Evaluate(p).Destructive, nil)
	if len(allowed) != 0 {
		t.Errorf("allowed = %+v, want none", allowed)
	}
	if len(refused) != 2 {
		t.Errorf("refused = %d, want both", len(refused))
	}
}

// A finding this gate could not TYPE must be refused whatever the allowlist
// says. Matching "" against the list would wave one through on the strength of a
// field that was never read.
func TestPartitionAllowedRefusesAnUntypedFinding(t *testing.T) {
	refused, allowed := PartitionAllowed(
		[]Finding{{Address: "unknown.thing", Kind: "destroy"}},
		[]string{"linode_lke_node_pool", ""})
	if len(allowed) != 0 || len(refused) != 1 {
		t.Errorf("refused=%+v allowed=%+v — an untyped finding must be refused", refused, allowed)
	}
}

// End to end through Run: the permitted destruction still has to be SAID, and
// said on the error stream, or the allowlist quietly stops being one.
func TestRunAnnouncesWhatTheAllowlistPermitted(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run("-", false, []string{"linode_lke_node_pool"}, &out, &errOut,
		strings.NewReader(poolResizeAndClusterRecyclePlan))
	if err == nil {
		t.Fatal("the LKE cluster recycle must still fail the check")
	}
	if !strings.Contains(errOut.String(), "::warning::linode_lke_node_pool.this would be replaced — PERMITTED") {
		t.Errorf("the permitted replace must be announced; got:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "1 live resource(s)") {
		t.Errorf("the count must be of the REFUSED findings, not all of them; got:\n%s", errOut.String())
	}
}

// A plan whose only destruction is allowlisted passes — otherwise a node-pool
// resize could never be applied.
func TestRunPassesWhenEveryDestructionIsAllowed(t *testing.T) {
	onlyPool := `{"format_version":"1.2","resource_changes":[
	 {"address":"linode_lke_node_pool.this","type":"linode_lke_node_pool",
	  "change":{"actions":["delete","create"]}}]}`
	var out, errOut bytes.Buffer
	if err := Run("-", false, []string{"linode_lke_node_pool"}, &out, &errOut,
		strings.NewReader(onlyPool)); err != nil {
		t.Fatalf("an allowlisted resize must pass: %v", err)
	}
	if !strings.Contains(errOut.String(), "PERMITTED") {
		t.Errorf("even a passing run must say what it waved through; got:\n%s", errOut.String())
	}
}

// THE COMPOSITE ALWAYS PASSES THE FLAG, empty or not, so the command line has one
// shape — `--allow-replace ""` is what three of the four apply lanes send. It has
// to mean "allow nothing", not "allow everything"; the inverse would silently
// disarm the guard on object-storage, databases and the shared VPC at once.
func TestEmptyAllowReplaceValueAllowsNothing(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run("-", false, []string{""}, &out, &errOut,
		strings.NewReader(poolResizeAndClusterRecyclePlan))
	if err == nil {
		t.Fatal(`--allow-replace "" must allow nothing, so both destructive changes stand`)
	}
	if !strings.Contains(errOut.String(), "2 live resource(s)") {
		t.Errorf("both findings must be refused; got:\n%s", errOut.String())
	}
	if strings.Contains(errOut.String(), "PERMITTED") {
		t.Errorf("nothing may be permitted by an empty allowlist; got:\n%s", errOut.String())
	}
}
