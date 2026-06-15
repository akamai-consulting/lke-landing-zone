package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fwCall records one stubbed kubectl invocation.
type fwCall struct {
	stdin string
	args  string // space-joined argv
}

// withFirewallKubectl swaps the kubectl seam, recording calls; fail (if set)
// decides per-argv whether an invocation errors.
func withFirewallKubectl(t *testing.T, fail func(args []string) error) *[]fwCall {
	t.Helper()
	orig := firewallKubectlFn
	calls := new([]fwCall)
	firewallKubectlFn = func(stdin string, args ...string) error {
		*calls = append(*calls, fwCall{stdin: stdin, args: strings.Join(args, " ")})
		if fail != nil {
			return fail(args)
		}
		return nil
	}
	t.Cleanup(func() { firewallKubectlFn = orig })
	return calls
}

// firewallTestEnv sets the command's full env contract: KUBECONFIG pointing at
// a real non-empty file plus overrides. Empty values clear a variable — the
// script's ${VAR:-} semantics treat empty as unset throughout, and the port
// must too.
func firewallTestEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kc, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"KUBECONFIG":           kc,
		"LINODE_FIREWALL_ID":   "12345",
		"CLOUD_FIREWALL_TOKEN": "cf-token",
		"LINODE_TOKEN":         "",
		"CLUSTER_ID":           "",
	}
	for k, v := range overrides {
		env[k] = v
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

// captureFirewallOutput runs fn with os.Stdout and os.Stderr redirected to
// pipes and returns what it printed to each.
func captureFirewallOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wo, we
	fn()
	wo.Close()
	we.Close()
	o, _ := io.ReadAll(ro)
	e, _ := io.ReadAll(re)
	return string(o), string(e)
}

func TestRunCIBootstrapCloudFirewallEnvValidation(t *testing.T) {
	emptyKC := filepath.Join(t.TempDir(), "empty-kubeconfig")
	if err := os.WriteFile(emptyKC, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		overrides  map[string]string
		wantErr    string
		wantStderr string
	}{
		{
			name:      "missing KUBECONFIG",
			overrides: map[string]string{"KUBECONFIG": ""},
			wantErr:   "KUBECONFIG must be set",
		},
		{
			name:      "missing LINODE_FIREWALL_ID",
			overrides: map[string]string{"LINODE_FIREWALL_ID": ""},
			wantErr:   "LINODE_FIREWALL_ID must be set",
		},
		{
			name:       "KUBECONFIG names a missing file",
			overrides:  map[string]string{"KUBECONFIG": filepath.Join(t.TempDir(), "nope")},
			wantErr:    "missing or empty",
			wantStderr: "::error::KUBECONFIG",
		},
		{
			name:       "KUBECONFIG names an empty file",
			overrides:  map[string]string{"KUBECONFIG": emptyKC},
			wantErr:    "missing or empty",
			wantStderr: "kubeconfig_raw",
		},
		{
			name:       "neither token set",
			overrides:  map[string]string{"CLOUD_FIREWALL_TOKEN": "", "LINODE_TOKEN": ""},
			wantErr:    "no Linode API token",
			wantStderr: "::error::Neither CLOUD_FIREWALL_TOKEN nor LINODE_TOKEN is set",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			firewallTestEnv(t, tc.overrides)
			calls := withFirewallKubectl(t, nil)
			var err error
			_, stderr := captureFirewallOutput(t, func() { err = runCIBootstrapCloudFirewall() })
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantStderr)
			}
			if len(*calls) != 0 {
				t.Errorf("validation failure must not touch kubectl, got %v", *calls)
			}
		})
	}
}

// unmarshalManifest decodes a stdin manifest/patch for structural assertions.
func unmarshalManifest(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("manifest %q is not valid JSON: %v", raw, err)
	}
	return m
}

func TestRunCIBootstrapCloudFirewallHappyPathWithClusterID(t *testing.T) {
	firewallTestEnv(t, map[string]string{"CLUSTER_ID": "67890"})
	calls := withFirewallKubectl(t, nil)
	var err error
	stdout, stderr := captureFirewallOutput(t, func() { err = runCIBootstrapCloudFirewall() })
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 5 {
		t.Fatalf("kubectl calls = %d (%v), want 5", len(*calls), *calls)
	}
	// Final call rolls the Deployment so it picks up the patched ConfigMap.
	if last := (*calls)[4].args; last != "rollout restart deployment "+firewallDeploymentName+" -n kube-system" {
		t.Errorf("call 5 argv = %q, want rollout restart of %s", last, firewallDeploymentName)
	}

	// 1. Secret apply: manifest over stdin, token off argv.
	if (*calls)[0].args != "apply -f -" {
		t.Errorf("call 1 argv = %q, want apply -f -", (*calls)[0].args)
	}
	if strings.Contains((*calls)[0].args, "cf-token") {
		t.Error("token leaked onto kubectl argv")
	}
	secret := unmarshalManifest(t, (*calls)[0].stdin)
	meta := secret["metadata"].(map[string]any)
	if secret["kind"] != "Secret" || meta["name"] != "linode" || meta["namespace"] != "kube-system" {
		t.Errorf("secret manifest = %v, want kube-system/linode Secret", secret)
	}
	if sd := secret["stringData"].(map[string]any); sd["token"] != "cf-token" {
		t.Errorf("secret stringData = %v, want token=cf-token", sd)
	}

	// 2. ConfigMap apply.
	if (*calls)[1].args != "apply -f -" {
		t.Errorf("call 2 argv = %q, want apply -f -", (*calls)[1].args)
	}
	cm := unmarshalManifest(t, (*calls)[1].stdin)
	cmMeta := cm["metadata"].(map[string]any)
	if cm["kind"] != "ConfigMap" || cmMeta["name"] != firewallConfigMapName || cmMeta["namespace"] != "kube-system" {
		t.Errorf("configmap manifest = %v, want kube-system/%s ConfigMap", cm, firewallConfigMapName)
	}

	// 3+4. Merge patches for LINODE_FIREWALL_ID then LKE_CLUSTER_ID.
	wantPatchArgv := "patch configmap " + firewallConfigMapName + " -n kube-system --type merge --patch "
	for i, want := range []string{`{"data":{"LINODE_FIREWALL_ID":"12345"}}`, `{"data":{"LKE_CLUSTER_ID":"67890"}}`} {
		c := (*calls)[2+i]
		if c.args != wantPatchArgv+want {
			t.Errorf("call %d argv = %q, want %q", 3+i, c.args, wantPatchArgv+want)
		}
		if c.stdin != "" {
			t.Errorf("call %d stdin = %q, want none", 3+i, c.stdin)
		}
	}

	for _, line := range []string{
		"Set LINODE_FIREWALL_ID=12345 in " + firewallConfigMapName,
		"Set LKE_CLUSTER_ID=67890 in " + firewallConfigMapName,
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, line)
		}
	}
	if strings.Contains(stdout, "CLUSTER_ID not provided") {
		t.Error("CLUSTER_ID was set — must not print the disabled-ACL line")
	}
	if strings.Contains(stderr, "::warning::") {
		t.Errorf("CLOUD_FIREWALL_TOKEN was set — no fallback warning expected, got %q", stderr)
	}
}

func TestRunCIBootstrapCloudFirewallPatchesVPCCIDR(t *testing.T) {
	firewallTestEnv(t, map[string]string{"VPC_CIDR": "10.0.0.0/13", "CLUSTER_ID": "67890"})
	calls := withFirewallKubectl(t, nil)
	var err error
	captureFirewallOutput(t, func() { err = runCIBootstrapCloudFirewall() })
	if err != nil {
		t.Fatal(err)
	}
	// secret apply, configmap apply, then patches in order:
	// LINODE_FIREWALL_ID, VPC_CIDR, LKE_CLUSTER_ID.
	if len(*calls) != 6 {
		t.Fatalf("kubectl calls = %d (%v), want 6 (incl. VPC_CIDR patch + rollout)", len(*calls), *calls)
	}
	wantPatch := "patch configmap " + firewallConfigMapName + " -n kube-system --type merge --patch "
	for i, want := range []string{
		`{"data":{"LINODE_FIREWALL_ID":"12345"}}`,
		`{"data":{"VPC_CIDR":"10.0.0.0/13"}}`,
		`{"data":{"LKE_CLUSTER_ID":"67890"}}`,
	} {
		if (*calls)[2+i].args != wantPatch+want {
			t.Errorf("patch call %d argv = %q, want %q", i, (*calls)[2+i].args, wantPatch+want)
		}
	}
}

func TestRunCIBootstrapCloudFirewallWithoutVPCCIDR(t *testing.T) {
	// VPC_CIDR unset → no VPC_CIDR patch (chart default stands).
	firewallTestEnv(t, nil)
	calls := withFirewallKubectl(t, nil)
	var err error
	captureFirewallOutput(t, func() { err = runCIBootstrapCloudFirewall() })
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range *calls {
		if strings.Contains(c.args, "VPC_CIDR") {
			t.Errorf("unexpected VPC_CIDR patch when env unset: %q", c.args)
		}
	}
}

func TestRunCIBootstrapCloudFirewallWithoutClusterID(t *testing.T) {
	firewallTestEnv(t, nil)
	calls := withFirewallKubectl(t, nil)
	var err error
	stdout, _ := captureFirewallOutput(t, func() { err = runCIBootstrapCloudFirewall() })
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 4 {
		t.Fatalf("kubectl calls = %d (%v), want 4 (no LKE_CLUSTER_ID patch; +rollout)", len(*calls), *calls)
	}
	for _, c := range *calls {
		if strings.Contains(c.args, "LKE_CLUSTER_ID") {
			t.Errorf("unexpected LKE_CLUSTER_ID patch: %q", c.args)
		}
	}
	if !strings.Contains(stdout, "CLUSTER_ID not provided — LKE control-plane ACL reconciliation will be disabled") {
		t.Errorf("stdout = %q, want the disabled-ACL notice", stdout)
	}
}

func TestRunCIBootstrapCloudFirewallTokenFallback(t *testing.T) {
	firewallTestEnv(t, map[string]string{"CLOUD_FIREWALL_TOKEN": "", "LINODE_TOKEN": "li-token"})
	calls := withFirewallKubectl(t, nil)
	var err error
	_, stderr := captureFirewallOutput(t, func() { err = runCIBootstrapCloudFirewall() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "::warning::CLOUD_FIREWALL_TOKEN not set — falling back to LINODE_TOKEN") {
		t.Errorf("stderr = %q, want the fallback warning", stderr)
	}
	secret := unmarshalManifest(t, (*calls)[0].stdin)
	if sd := secret["stringData"].(map[string]any); sd["token"] != "li-token" {
		t.Errorf("secret stringData = %v, want the LINODE_TOKEN fallback", sd)
	}
}

func TestRunCIBootstrapCloudFirewallPrefersCloudFirewallToken(t *testing.T) {
	firewallTestEnv(t, map[string]string{"CLOUD_FIREWALL_TOKEN": "cf-token", "LINODE_TOKEN": "li-token"})
	calls := withFirewallKubectl(t, nil)
	var err error
	_, stderr := captureFirewallOutput(t, func() { err = runCIBootstrapCloudFirewall() })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "::warning::") {
		t.Errorf("both tokens set — no warning expected, got %q", stderr)
	}
	secret := unmarshalManifest(t, (*calls)[0].stdin)
	if sd := secret["stringData"].(map[string]any); sd["token"] != "cf-token" {
		t.Errorf("secret stringData = %v, want CLOUD_FIREWALL_TOKEN preferred", sd)
	}
}

func TestRunCIBootstrapCloudFirewallKubectlFailurePropagates(t *testing.T) {
	cases := []struct {
		name      string
		failOn    func(args []string) bool
		wantErr   string
		wantCalls int
	}{
		{
			name:      "secret apply fails",
			failOn:    func(args []string) bool { return args[0] == "apply" },
			wantErr:   "apply kube-system/linode Secret",
			wantCalls: 1,
		},
		{
			name:      "configmap patch fails",
			failOn:    func(args []string) bool { return args[0] == "patch" },
			wantErr:   "patch LINODE_FIREWALL_ID into " + firewallConfigMapName,
			wantCalls: 3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			firewallTestEnv(t, map[string]string{"CLUSTER_ID": "67890"})
			calls := withFirewallKubectl(t, func(args []string) error {
				if tc.failOn(args) {
					return errors.New("exit status 1")
				}
				return nil
			})
			var err error
			captureFirewallOutput(t, func() { err = runCIBootstrapCloudFirewall() })
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want %q", err, tc.wantErr)
			}
			if len(*calls) != tc.wantCalls {
				t.Errorf("kubectl calls = %d (%v), want %d (stop at the failure)", len(*calls), *calls, tc.wantCalls)
			}
		})
	}
}

func TestFirewallManifestHelpers(t *testing.T) {
	// The token must survive JSON-hostile characters (quotes, backslashes).
	hostile := `to"k\en` + "\n"
	secret := unmarshalManifest(t, firewallSecretManifest(hostile))
	if sd := secret["stringData"].(map[string]any); sd["token"] != hostile {
		t.Errorf("token round-trip = %q, want %q", sd["token"], hostile)
	}
	if secret["apiVersion"] != "v1" || secret["type"] != "Opaque" {
		t.Errorf("secret manifest = %v, want v1 Opaque", secret)
	}

	cm := unmarshalManifest(t, firewallConfigMapManifest())
	if cm["apiVersion"] != "v1" || cm["kind"] != "ConfigMap" {
		t.Errorf("configmap manifest = %v, want a v1 ConfigMap", cm)
	}
	if _, hasData := cm["data"]; hasData {
		t.Errorf("configmap manifest = %v, want no data (values are patched in)", cm)
	}

	patch := unmarshalManifest(t, firewallConfigPatch("LINODE_FIREWALL_ID", "42"))
	if data := patch["data"].(map[string]any); data["LINODE_FIREWALL_ID"] != "42" {
		t.Errorf("patch = %v, want data.LINODE_FIREWALL_ID=42", patch)
	}
}

func TestCIBootstrapCloudFirewallCmd(t *testing.T) {
	c := ciBootstrapCloudFirewallCmd()
	if c.Use != "bootstrap-cloud-firewall" {
		t.Errorf("Use = %q, want bootstrap-cloud-firewall", c.Use)
	}
	if !strings.Contains(c.Long, "Native port of bootstrap-cloud-firewall.sh") {
		t.Errorf("Long = %q, want the native-port preamble", c.Long)
	}

	// Execute end-to-end through cobra so the RunE closure is exercised.
	firewallTestEnv(t, nil)
	calls := withFirewallKubectl(t, nil)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	var err error
	captureFirewallOutput(t, func() { err = c.Execute() })
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 4 {
		t.Errorf("kubectl calls = %d, want 4", len(*calls))
	}
}
