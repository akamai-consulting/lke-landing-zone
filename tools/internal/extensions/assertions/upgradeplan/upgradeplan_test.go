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
	err := Run("-", &out, &errOut,
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
	if err := Run("-", &out, &errOut,
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
	if err := Run("-", &out, &errOut,
		strings.NewReader(`{"format_version":"1.2","resource_changes":[]}`)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "state key") {
		t.Errorf("an empty plan must point at the likeliest cause of an unexpected one:\n%s", out.String())
	}
}

func TestRunSurfacesAnUnreadablePlan(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := Run("/nonexistent/plan.json", &out, &errOut, strings.NewReader("")); err == nil {
		t.Error("an unreadable plan must be an error, not an empty verdict")
	}
}
