package main

import "testing"

// kubeconfigRawProblem gates what fetch-kubeconfig-state writes to disk: a
// corrupt value poisons every downstream kubectl with "yaml: control characters
// are not allowed" and masks the real failure the diag step exists to surface.
func TestKubeconfigRawProblem(t *testing.T) {
	valid := "apiVersion: v1\nkind: Config\nclusters:\n- name: lke\n  cluster:\n    server: https://x:6443\n"
	if got := kubeconfigRawProblem([]byte(valid)); got != "" {
		t.Errorf("valid kubeconfig flagged: %q", got)
	}
	cases := map[string]string{
		"empty":            "",
		"whitespace":       "   \n\t ",
		"control chars":    "apiVersion: v1\nclusters:\x00\x07 garbage",
		"not a kubeconfig": "just some\nrandom: text\n", // no apiVersion
	}
	for name, in := range cases {
		if kubeconfigRawProblem([]byte(in)) == "" {
			t.Errorf("%s: should be flagged as a problem, was accepted", name)
		}
	}
	// A valid kubeconfig with a tab/CR (legal YAML whitespace) is NOT rejected.
	if got := kubeconfigRawProblem([]byte("apiVersion: v1\r\nclusters: []\r\n")); got != "" {
		t.Errorf("CRLF kubeconfig wrongly flagged: %q", got)
	}
}
