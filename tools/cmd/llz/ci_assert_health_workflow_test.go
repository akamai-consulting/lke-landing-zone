package main

import (
	"encoding/json"
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
