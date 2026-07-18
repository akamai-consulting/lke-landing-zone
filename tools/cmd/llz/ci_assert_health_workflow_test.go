package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealthWorkflowManifest(t *testing.T) {
	raw := healthWorkflowManifest("llz-cluster-health", "llz-argo-workflows")
	var doc struct {
		Kind     string `json:"kind"`
		Metadata struct {
			GenerateName string `json:"generateName"`
			Namespace    string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			WorkflowTemplateRef struct {
				Name string `json:"name"`
			} `json:"workflowTemplateRef"`
			Arguments struct {
				Parameters []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"parameters"`
			} `json:"arguments"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\n%s", err, raw)
	}
	if doc.Kind != "Workflow" {
		t.Errorf("kind = %q, want Workflow", doc.Kind)
	}
	if doc.Metadata.GenerateName == "" {
		t.Error("generateName must be set so repeated runs never collide")
	}
	if doc.Metadata.Namespace != "llz-argo-workflows" {
		t.Errorf("namespace = %q, want llz-argo-workflows", doc.Metadata.Namespace)
	}
	if doc.Spec.WorkflowTemplateRef.Name != "llz-cluster-health" {
		t.Errorf("workflowTemplateRef.name = %q, want llz-cluster-health", doc.Spec.WorkflowTemplateRef.Name)
	}
	// Gate mode: a genuinely unhealthy cluster must fail the run post-converge.
	if len(doc.Spec.Arguments.Parameters) != 1 ||
		doc.Spec.Arguments.Parameters[0].Name != "fail-on-unhealthy" ||
		doc.Spec.Arguments.Parameters[0].Value != "true" {
		t.Errorf("want a single fail-on-unhealthy=true parameter, got %+v", doc.Spec.Arguments.Parameters)
	}
}

func TestCreatedWorkflowName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"generated name", `{"metadata":{"name":"e2e-assert-health-abc12"}}`, "e2e-assert-health-abc12", true},
		{"missing name", `{"metadata":{}}`, "", false},
		{"garbage", `not json`, "", false},
		{"empty", ``, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := createdWorkflowName([]byte(tc.in))
			if got != tc.want || ok != tc.ok {
				t.Errorf("createdWorkflowName(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestWorkflowPhase(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"running", `{"status":{"phase":"Running"}}`, "Running"},
		{"succeeded", `{"status":{"phase":"Succeeded"}}`, "Succeeded"},
		{"no status yet", `{"metadata":{"name":"x"}}`, ""},
		{"garbage", `nope`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflowPhase([]byte(tc.in)); got != tc.want {
				t.Errorf("workflowPhase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestClassifyWorkflowPhase(t *testing.T) {
	cases := []struct {
		phase          string
		terminal, succ bool
	}{
		{"Succeeded", true, true},
		{"Failed", true, false},
		{"Error", true, false},
		{"Running", false, false},
		{"Pending", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			terminal, succ := classifyWorkflowPhase(tc.phase)
			if terminal != tc.terminal || succ != tc.succ {
				t.Errorf("classifyWorkflowPhase(%q) = (%v,%v), want (%v,%v)", tc.phase, terminal, succ, tc.terminal, tc.succ)
			}
		})
	}
}

// healthTransientOnly gates the health-gate retry: ONLY "0 hard-failed with work
// in progress" (a cluster mid-settle, e.g. the argocd-redis WRONGPASS flap right
// after an operator roll) retries; hard failures and unparseable logs fail fast.
func TestHealthTransientOnly(t *testing.T) {
	cases := []struct {
		name string
		logs string
		want bool
	}{
		{"in-progress only (the live 29547902622 failure)", "PENDING x (Unknown/Healthy) — argocd-redis cache auth\nconvergence: 0 hard-failed, 9 in-progress\nError: exit status 2", true},
		{"hard failures present", "convergence: 2 hard-failed, 3 in-progress", false},
		{"hard failures, nothing pending", "convergence: 1 hard-failed, 0 in-progress", false},
		{"fully converged (should not fail at all)", "convergence: 0 hard-failed, 0 in-progress", false},
		{"no verdict line (crash, OOM, missing logs)", "panic: boom", false},
		{"empty logs", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := healthTransientOnly(c.logs); got != c.want {
				t.Errorf("healthTransientOnly(%q) = %v, want %v", c.logs, got, c.want)
			}
		})
	}
}

// TestHealthWorkflowExpected pins the anchoring fix: whether an absent
// WorkflowTemplate is a failure is decided by the SPEC, never by the cluster.
// Anchoring it to the cluster is what let a component the e2e explicitly enables
// fail to deploy and still report green.
func TestHealthWorkflowExpected(t *testing.T) {
	// A spec with clusterHealthWorkflow enabled for "e2e" and absent (default
	// disabled) for "lab" — the two cases that must diverge.
	dir := t.TempDir()
	mustWrite := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("landingzone.yaml", `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: LandingZone
metadata: { name: t }
spec:
  instance: { upstreamOrg: akamai-consulting, repo: o/t, forge: github, templateVersion: v0.4.0 }
  defaults:
    cluster:
      k8sVersion: v1.33.6+lke7
      nodePool: { type: g8-dedicated-8-4, count: 3 }
`)
	env := func(name, components string) string {
		return `
apiVersion: llz.akamai-consulting.io/v1alpha1
kind: ClusterDefinition
metadata: { name: ` + name + ` }
spec:
  cluster:
    clusterLabel: c-` + name + `
    region: us-sea
    bootstrap: { name: b-` + name + ` }
    objectStorage: { cluster: us-sea-1 }
` + components
	}
	mustWrite("environments/e2e.yaml", env("e2e", "  components:\n    clusterHealthWorkflow: { enabled: true }\n"))
	mustWrite("environments/lab.yaml", env("lab", ""))

	tests := []struct {
		name       string
		region     string
		chdir      bool
		wantExpect bool
		wantWhy    string
	}{
		{
			// THE regression this fixes: enabled in spec + template missing = deploy
			// failure, which must fail rather than read as "component disabled".
			name:   "enabled in spec => absent template is a failure",
			region: "e2e", chdir: true, wantExpect: true, wantWhy: "IS enabled",
		},
		{
			name:   "disabled in spec => absent template is a legitimate no-op",
			region: "lab", chdir: true, wantExpect: false, wantWhy: "is disabled",
		},
		{
			name:   "unknown deployment => cannot claim it is expected",
			region: "nope", chdir: true, wantExpect: false, wantWhy: "not a deployment",
		},
		{
			// The one remaining hole, now explicit and stated in the output rather
			// than being every caller's silent default.
			name:   "no --region => unanchored, and says so",
			region: "", chdir: true, wantExpect: false, wantWhy: "no --region",
		},
		{
			name:   "unreadable spec => not expected, so ad-hoc runs still work",
			region: "e2e", chdir: false, wantExpect: false, wantWhy: "could not be read",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.chdir {
				t.Chdir(dir)
			} else {
				t.Chdir(t.TempDir()) // no landingzone.yaml here
			}
			got, why := healthWorkflowExpected(tt.region)
			if got != tt.wantExpect {
				t.Errorf("expected = %v, want %v (why: %s)", got, tt.wantExpect, why)
			}
			if !strings.Contains(why, tt.wantWhy) {
				t.Errorf("reason %q should explain itself with %q", why, tt.wantWhy)
			}
		})
	}
}
