package clusterspec

import (
	"strings"
	"testing"
)

// The guard is worthless if its regex does not match what the renderers actually
// emit — an inert check reads exactly like a clean one. So every test below feeds
// REAL Render*Patch output rather than a hand-written body, and this first test
// asserts the pairs are found at all.
func TestCheckDerivedEnvValuesSeesRealRendererOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string // env var names the scanner must find
	}{
		{
			name: "harbor",
			body: RenderHarborHostPatch("lab.example.com", "acme/instance"),
			want: []string{"HARBOR_HOST", "GH_REPO"},
		},
		{
			name: "broadPatRotator",
			body: RenderBroadPATEnvPatch("llz-broad", "primary,secondary", "acme/instance"),
			want: []string{"BROAD_PAT_LABEL", "BROAD_PAT_DEPLOYMENTS", "GH_REPO"},
		},
		{
			name: "llzReconciler",
			body: RenderReconcilerEnvPatch("pri", "primary", "us-ord-1", "acme/instance", "acme"),
			want: []string{"REGION_SHORT", "REGION", "OBJ_CLUSTER", "GH_REPO"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := map[string]bool{}
			for _, m := range envPairRe.FindAllStringSubmatch(tc.body, -1) {
				found[m[1]] = true
			}
			for _, w := range tc.want {
				if !found[w] {
					t.Errorf("scanner did not find %s in the rendered patch — the guard is inert:\n%s", w, tc.body)
				}
			}
			// A well-formed render must pass cleanly.
			if errs := CheckDerivedEnvValues(tc.body); len(errs) > 0 {
				t.Errorf("valid render rejected: %v", errs)
			}
		})
	}
}

// The regression this whole file exists for. Post-#342 HarborHost("") returns "",
// so the CURRENT renderer is clean on managed — and that empty must stay ACCEPTED,
// because it is what routes the provisioner into systeminfo discovery. The
// malformed "harbor." shape (what shipped before the fix) must be rejected.
func TestCheckDerivedEnvValuesHarborHostRegression(t *testing.T) {
	// Managed: no domainSuffix. Empty HARBOR_HOST is correct, not a miss.
	managed := RenderHarborHostPatch("", "acme/instance")
	if !strings.Contains(managed, `value: ""`) {
		t.Fatalf("expected an empty HARBOR_HOST on managed, got:\n%s", managed)
	}
	if errs := CheckDerivedEnvValues(managed); len(errs) > 0 {
		t.Errorf("empty HARBOR_HOST is the discovery signal and must be accepted, got %v", errs)
	}

	// The pre-fix shape, reconstructed: non-empty, trailing dot, defeats every
	// `!= ""` guard between render and the registry.
	broken := strings.Replace(managed, `value: ""`, `value: "harbor."`, 1)
	errs := CheckDerivedEnvValues(broken)
	if len(errs) == 0 {
		t.Fatal(`"harbor." must be rejected — this is the exact value that shipped to every managed instance`)
	}
	if !strings.Contains(errs[0].Error(), "HARBOR_HOST") || !strings.Contains(errs[0].Error(), "empty DNS label") {
		t.Errorf("error should name the field and the defect, got: %v", errs[0])
	}
}

func TestCheckDerivedEnvValuesRejectsMalformedFields(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantSubstr string
	}{
		{
			name:       "truncated repo slug",
			body:       RenderHarborHostPatch("lab.example.com", "acme/"),
			wantSubstr: "GH_REPO",
		},
		{
			name:       "repo slug with no owner",
			body:       RenderHarborHostPatch("lab.example.com", "/instance"),
			wantSubstr: "not an <owner>/<name> slug",
		},
		{
			name:       "leading-dot host",
			body:       strings.Replace(RenderHarborHostPatch("x", "a/b"), `value: "harbor.x"`, `value: ".x"`, 1),
			wantSubstr: "empty DNS label",
		},
		{
			name:       "doubled dot",
			body:       strings.Replace(RenderHarborHostPatch("x", "a/b"), `value: "harbor.x"`, `value: "harbor..x"`, 1),
			wantSubstr: "empty DNS label",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := CheckDerivedEnvValues(tc.body)
			if len(errs) == 0 {
				t.Fatalf("expected a rejection, body:\n%s", tc.body)
			}
			joined := errsText(errs)
			if !strings.Contains(joined, tc.wantSubstr) {
				t.Errorf("want %q in %q", tc.wantSubstr, joined)
			}
		})
	}
}

// A sentinel reaching a rendered patch means a substitution silently did not
// happen — caught whether or not the field has a declared rule.
func TestCheckDerivedEnvValuesRejectsPlaceholderSentinels(t *testing.T) {
	for _, sentinel := range []string{"REPLACE_ME", "REPLACE_PER_ENV", "placeholder.example.com"} {
		body := strings.Replace(RenderHarborHostPatch("x", "a/b"), `value: "harbor.x"`, `value: "`+sentinel+`"`, 1)
		errs := CheckDerivedEnvValues(body)
		if len(errs) == 0 {
			t.Errorf("%s must be rejected", sentinel)
			continue
		}
		if !strings.Contains(errsText(errs), "never substituted") {
			t.Errorf("%s: want a substitution error, got %v", sentinel, errs)
		}
	}

	// An UNDECLARED field still gets the sentinel check — the point of scanning
	// output rather than wiring each renderer in by hand.
	unknown := "        - name: SOME_FUTURE_FIELD\n          value: \"REPLACE_ME\"\n"
	if errs := CheckDerivedEnvValues(unknown); len(errs) == 0 {
		t.Error("a sentinel in an undeclared field must still be rejected")
	}
}

// Empty is ALWAYS allowed — the guard is about malformed-but-non-empty, and
// enforcing presence here would reject valid instances. OBJ_CLUSTER is empty
// whenever an env declares no object storage (validate.OBJClusterID("") == nil),
// and HARBOR_HOST is empty by design on managed. Regression-locked, because
// tightening this is a tempting and wrong "improvement".
func TestCheckDerivedEnvValuesAllowsEveryEmptyValue(t *testing.T) {
	for _, body := range []string{
		RenderReconcilerEnvPatch("", "", "", "", "acme"),
		RenderHarborHostPatch("", ""),
		RenderBroadPATEnvPatch("", "", ""),
	} {
		if errs := CheckDerivedEnvValues(body); len(errs) > 0 {
			t.Errorf("empty values must be accepted (spec completeness is llz validate's job), got %v", errs)
		}
	}
}

// Every offending field is reported in one pass, so a render does not turn into a
// fix-one-rerun loop.
func TestCheckDerivedEnvValuesReportsEveryOffender(t *testing.T) {
	body := RenderReconcilerEnvPatch("pri ", "prim ary", "us ord", "acme/", "acme")
	errs := CheckDerivedEnvValues(body)
	if len(errs) < 4 {
		t.Fatalf("expected all four fields reported, got %d: %v", len(errs), errs)
	}
	joined := errsText(errs)
	for _, want := range []string{"REGION_SHORT", "REGION", "OBJ_CLUSTER", "GH_REPO"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %q", want, joined)
		}
	}
}

func TestShapeHelpers(t *testing.T) {
	for _, ok := range []string{"harbor.lab.example.com", "us-ord-1", "primary"} {
		if err := hostShape(ok); err != nil {
			t.Errorf("hostShape(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"harbor.", ".lab", "a..b", "has space"} {
		if err := hostShape(bad); err == nil {
			t.Errorf("hostShape(%q) = nil, want an error", bad)
		}
	}
	if err := repoSlugShape("acme/instance"); err != nil {
		t.Errorf("repoSlugShape valid = %v", err)
	}
	for _, bad := range []string{"acme", "acme/", "/x", "a/b/c", "acme /x"} {
		if err := repoSlugShape(bad); err == nil {
			t.Errorf("repoSlugShape(%q) = nil, want an error", bad)
		}
	}
}

func errsText(errs []error) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// The documented wire format is SPACE-separated (spec doc + the CronJob comment),
// and the consumer parses it with strings.Fields — but the guard used tokenShape,
// which rejects all whitespace. So the only multi-deployment value that rendered
// was a comma list, which strings.Fields reads as one name, and the rotator then
// targets a GitHub environment called `infra-primary,secondary` AFTER stamping
// rotated_at — suppressing the retry for 60 days.
func TestBroadPatDeploymentsAcceptsTheFormatItsConsumerParses(t *testing.T) {
	pair := func(v string) string {
		return "            - name: BROAD_PAT_DEPLOYMENTS\n              value: \"" + v + "\"\n"
	}
	for _, good := range []string{"primary secondary", "primary", "a b c"} {
		if errs := CheckDerivedEnvValues(pair(good)); len(errs) != 0 {
			t.Errorf("%q must render: %v", good, errs)
		}
	}
	// Still rejected: padded and whitespace-only, which are malformed rather than
	// multi-valued.
	for _, bad := range []string{" primary", "primary ", "  "} {
		if errs := CheckDerivedEnvValues(pair(bad)); len(errs) == 0 {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
