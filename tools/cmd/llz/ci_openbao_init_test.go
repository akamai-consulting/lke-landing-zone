package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withGHSetSecret swaps the gh-secret seam, recording "name@env" calls.
func withGHSetSecret(t *testing.T, fail func(name string) error) *[]string {
	t.Helper()
	orig := ghSetSecretFn
	calls := new([]string)
	ghSetSecretFn = func(name, ghEnv, value string) error {
		*calls = append(*calls, name+"@"+ghEnv+"="+value)
		if fail != nil {
			return fail(name)
		}
		return nil
	}
	t.Cleanup(func() { ghSetSecretFn = orig })
	return calls
}

const initJSON = `{"unseal_keys_b64":["uk1","uk2","uk3","uk4","uk5"],"root_token":"s.root"}`

func TestParseBaoInit(t *testing.T) {
	r, err := parseBaoInit(initJSON)
	if err != nil || r.RootToken != "s.root" || len(r.UnsealKeysB64) != 5 {
		t.Fatalf("parseBaoInit = (%+v, %v), want full payload", r, err)
	}
	for _, bad := range []string{
		"", "not json",
		`{"unseal_keys_b64":["a","b"],"root_token":"s.x"}`, // too few shares
		`{"unseal_keys_b64":["a","b","c","d","e"]}`,        // no root
	} {
		if _, err := parseBaoInit(bad); err == nil {
			t.Errorf("parseBaoInit(%q) = nil error, want failure", bad)
		}
	}
}

func TestRunCIBaoInit(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY"} {
		t.Setenv(v, filepath.Join(dir, v))
	}
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		want := "operator init -key-shares=5 -key-threshold=3 -format=json"
		if pod != "platform-openbao-0" || strings.Join(args, " ") != want {
			t.Errorf("init exec = %s %v", pod, args)
		}
		return initJSON, "", nil
	})
	ghCalls := withGHSetSecret(t, nil)

	if err := runCIBaoInit(globalOpts{}, "primary"); err != nil {
		t.Fatal(err)
	}

	env, _ := os.ReadFile(filepath.Join(dir, "GITHUB_ENV"))
	wantEnv := "OPENBAO_ROOT_TOKEN=s.root\nUNSEAL_K1=uk1\nUNSEAL_K2=uk2\nUNSEAL_K3=uk3\n"
	if string(env) != wantEnv {
		t.Errorf("GITHUB_ENV = %q, want %q", env, wantEnv)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "GITHUB_OUTPUT"))
	if string(out) != "did_init=true\n" {
		t.Errorf("GITHUB_OUTPUT = %q, want did_init=true", out)
	}
	summary, _ := os.ReadFile(filepath.Join(dir, "GITHUB_STEP_SUMMARY"))
	if !strings.Contains(string(summary), "Save These Keys Now") || !strings.Contains(string(summary), initJSON) {
		t.Errorf("summary missing banner or init payload: %q", summary)
	}
	want := []string{
		"OPENBAO_UNSEAL_KEY_1@infra-primary=uk1",
		"OPENBAO_UNSEAL_KEY_2@infra-primary=uk2",
		"OPENBAO_UNSEAL_KEY_3@infra-primary=uk3",
		"OPENBAO_ROOT_TOKEN@infra-primary=s.root",
	}
	if strings.Join(*ghCalls, " ") != strings.Join(want, " ") {
		t.Errorf("gh calls = %v, want %v", *ghCalls, want)
	}
}

func TestRunCIBaoInitSummaryBeforeGHFailure(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"GITHUB_ENV", "GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY"} {
		t.Setenv(v, filepath.Join(dir, v))
	}
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return initJSON, "", nil
	})
	withGHSetSecret(t, func(string) error { return errors.New("403 secrets: write denied") })

	if err := runCIBaoInit(globalOpts{}, "primary"); err == nil {
		t.Fatal("want error when gh secret set fails")
	}
	// The one-shot init payload must already be in the summary regardless.
	summary, _ := os.ReadFile(filepath.Join(dir, "GITHUB_STEP_SUMMARY"))
	if !strings.Contains(string(summary), initJSON) {
		t.Error("init payload not captured in summary before the gh failure")
	}
}

func TestRunCIBaoInitRequiresRegionAndInitSuccess(t *testing.T) {
	if err := runCIBaoInit(globalOpts{}, ""); err == nil {
		t.Error("missing --region accepted")
	}
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return "", "Error initializing: Vault is already initialized", errors.New("exit status 2")
	})
	err := runCIBaoInit(globalOpts{}, "primary")
	if err == nil || !strings.Contains(err.Error(), "already initialized") {
		t.Errorf("err = %v, want operator-init failure with stderr", err)
	}
}

func TestRunCIBaoRegenRootValidTokenSkips(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.current")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		switch args[0] {
		case "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case "token":
			if token != "s.current" {
				t.Errorf("lookup used token %q", token)
			}
			return `{"data":{"policies":["root"]}}`, "", nil
		}
		t.Errorf("unexpected exec %v", args)
		return "", "", nil
	})
	ghCalls := withGHSetSecret(t, nil)
	if err := runCIBaoRegenRoot(globalOpts{}, "primary"); err != nil {
		t.Fatal(err)
	}
	if len(*ghCalls) != 0 {
		t.Errorf("valid token must not touch gh secrets: %v", *ghCalls)
	}
}

func TestRunCIBaoRegenRootSealedLeaderFails(t *testing.T) {
	withBaoExec(t, func(string, string, string, ...string) (string, string, error) {
		return `{"initialized":true,"sealed":true}`, "", errors.New("exit status 2")
	})
	if err := runCIBaoRegenRoot(globalOpts{}, "primary"); err == nil || !strings.Contains(err.Error(), "not unsealed") {
		t.Errorf("err = %v, want sealed-leader refusal", err)
	}
}

func TestRunCIBaoRegenRootFullQuorumFlow(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.revoked")
	t.Setenv("UNSEAL_K1", "k1")
	t.Setenv("UNSEAL_K2", "k2")
	t.Setenv("UNSEAL_K3", "k3")
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("GITHUB_ENV", envFile)

	keysSubmitted := 0
	cancelled := false
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case args[0] == "token": // revoked
			return "", "Code: 403. * permission denied", errors.New("exit status 2")
		case strings.Contains(cmd, "-cancel"):
			cancelled = true
			return "", "", nil
		case strings.Contains(cmd, "-init"):
			return `{"nonce":"n-1","otp":"otp-1"}`, "", nil
		case strings.Contains(cmd, "-nonce=n-1"):
			if args[len(args)-1] != "-" || stdin == "" {
				t.Errorf("unseal key must ride stdin, got args=%v stdin=%q", args, stdin)
			}
			keysSubmitted++
			if keysSubmitted == 3 {
				return fmt.Sprintf(`{"complete":true,"progress":3,"required":3,"encoded_token":"enc-%s"}`, strings.TrimSpace(stdin)), "", nil
			}
			return fmt.Sprintf(`{"complete":false,"progress":%d,"required":3}`, keysSubmitted), "", nil
		case strings.Contains(cmd, "-decode=enc-k3"):
			return `{"token":"s.newroot"}`, "", nil
		}
		t.Errorf("unexpected exec %v", args)
		return "", "", errors.New("unexpected")
	})
	ghCalls := withGHSetSecret(t, nil)

	if err := runCIBaoRegenRoot(globalOpts{}, "secondary"); err != nil {
		t.Fatal(err)
	}
	if !cancelled || keysSubmitted != 3 {
		t.Errorf("cancelled=%v keysSubmitted=%d, want cancel + 3 submissions", cancelled, keysSubmitted)
	}
	env, _ := os.ReadFile(envFile)
	if string(env) != "OPENBAO_ROOT_TOKEN=s.newroot\n" {
		t.Errorf("GITHUB_ENV = %q, want new root export", env)
	}
	if len(*ghCalls) != 1 || (*ghCalls)[0] != "OPENBAO_ROOT_TOKEN@infra-secondary=s.newroot" {
		t.Errorf("gh calls = %v, want one root-token write to infra-secondary", *ghCalls)
	}
}

func TestRunCIBaoRegenRootQuorumWithoutToken(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.revoked")
	t.Setenv("UNSEAL_K1", "k1")
	t.Setenv("UNSEAL_K2", "k2")
	t.Setenv("UNSEAL_K3", "k3")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case args[0] == "status":
			return `{"initialized":true,"sealed":false}`, "", nil
		case args[0] == "token":
			return "", "", errors.New("exit status 2")
		case strings.Contains(cmd, "-init"):
			return `{"nonce":"n-1","otp":"otp-1"}`, "", nil
		case strings.Contains(cmd, "-nonce"):
			// Wrong keys: progress advances but never completes.
			return `{"complete":false,"progress":1,"required":3}`, "", nil
		}
		return "", "", nil
	})
	withGHSetSecret(t, nil)
	err := runCIBaoRegenRoot(globalOpts{}, "primary")
	if err == nil || !strings.Contains(err.Error(), "encoded_token") {
		t.Errorf("err = %v, want missing-encoded_token failure", err)
	}
}

func TestRunCIBaoRegenRootMissingKeys(t *testing.T) {
	t.Setenv("OPENBAO_ROOT_TOKEN", "s.revoked")
	t.Setenv("UNSEAL_K1", "")
	t.Setenv("UNSEAL_K2", "")
	t.Setenv("UNSEAL_K3", "")
	withBaoExec(t, func(pod, token, stdin string, args ...string) (string, string, error) {
		if args[0] == "status" {
			return `{"initialized":true,"sealed":false}`, "", nil
		}
		return "", "", errors.New("exit status 2")
	})
	if err := runCIBaoRegenRoot(globalOpts{}, "primary"); err == nil || !strings.Contains(err.Error(), "UNSEAL_K1") {
		t.Errorf("err = %v, want missing-keys error", err)
	}
}
