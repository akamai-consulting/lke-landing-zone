package main

// regenroot.go ports regenerate-openbao-root.sh into `llz openbao regen-root`:
// the standard `bao operator generate-root` quorum flow (3-of-5 unseal-key
// holders) plus an optional re-seed of the infra-<region> OPENBAO_ROOT_TOKEN
// env secret. OpenBao's API is only reachable in-cluster, so — like the bash —
// this drives the bao CLI via `kubectl exec` against the active raft leader; the
// Go wins are that the binary travels with the operator (no instance-scripts/
// checkout needed) and the unseal keys are read in terminal raw mode (never
// echoed, never on argv, never on disk).

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/forge"
	"golang.org/x/term"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

const openbaoNS = "llz-openbao"

type regenRootOpts struct {
	updateGHA bool
	repo      string
}

func runRegenRoot(g globalOpts, region string, o regenRootOpts) error {
	if region == "" {
		return fmt.Errorf("usage: llz openbao regen-root <region> [--update-gha-secret] [--repo owner/repo]")
	}
	pod := findLeaderPod()
	ctx, _ := execOutput("kubectl", "config", "current-context")
	fmt.Printf("kubectl context: %s\n", strings.TrimSpace(string(ctx)))
	fmt.Printf("Target pod:      %s/%s (active raft leader)\n", openbaoNS, pod)
	fmt.Printf("Region (for GHA env name only): %s\n\n", region)
	if g.dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would run the bao generate-root quorum flow against the leader pod")
		return nil
	}

	// Sanity: reachable + unsealed.
	statusOut, _, err := baoExec(pod, "", "", "status", "-format=json")
	if err != nil {
		return fmt.Errorf("cannot reach OpenBao at %s/%s via the current kubectl context", openbaoNS, pod)
	}
	sealed, threshold := parseBaoStatus(statusOut)
	if sealed {
		return fmt.Errorf("%s is sealed — unseal it first, then re-run", pod)
	}
	fmt.Printf("OpenBao unsealed. Unseal threshold: %d.\n", threshold)

	// Clean slate, then init.
	_, _, _ = baoExec(pod, "", "", "operator", "generate-root", "-cancel")
	initOut, _, err := baoExec(pod, "", "", "operator", "generate-root", "-init", "-format=json")
	if err != nil {
		return fmt.Errorf("initialize generate-root: %w", err)
	}
	nonce, otp := parseGenRootInit(initOut)
	if nonce == "" || otp == "" {
		return fmt.Errorf("failed to initialize generate-root: %s", strings.TrimSpace(initOut))
	}
	fmt.Printf("Root regeneration started. nonce=%s\n\n", nonce)
	fmt.Printf("Each of the %d unseal-key holders should now paste their key.\n", threshold)
	fmt.Println("Keys are read silently and never written to disk.")

	// Submit keys (raw-mode read; piped via stdin so the key never hits argv).
	var encoded string
	for progress := 1; encoded == ""; progress++ {
		fmt.Printf("Unseal key #%d of %d: ", progress, threshold)
		key, err := readSecretLine()
		fmt.Println()
		if err != nil {
			return fmt.Errorf("reading unseal key: %w", err)
		}
		if key == "" {
			fmt.Println("  (empty input — retry)")
			progress--
			continue
		}
		out, errOut, err := baoExec(pod, "", key+"\n",
			"operator", "generate-root", "-nonce="+nonce, "-format=json", "-")
		key = ""
		if err != nil {
			_, _, _ = baoExec(pod, "", "", "operator", "generate-root", "-cancel")
			return fmt.Errorf("generate-root rejected key #%d: %s\n"+
				"  (wrong/duplicate key, or keys from a different OpenBao init — compare cluster_id)",
				progress, strings.TrimSpace(firstNonEmpty(errOut, out)))
		}
		complete, p, r, enc := parseGenRootStep(out)
		fmt.Printf("  Progress: %d/%d\n", p, r)
		if complete {
			encoded = enc
		}
	}
	if encoded == "" {
		return fmt.Errorf("generate-root completed but returned no encoded_token")
	}

	// Decode (local op against the OTP) inside the pod for binary parity.
	decodeOut, _, _ := baoExec(pod, "", "", "operator", "generate-root", "-decode="+encoded, "-otp="+otp, "-format=json")
	newRoot := parseTokenField(decodeOut)
	if newRoot == "" { // older bao prints a bare token
		bare, _, _ := baoExec(pod, "", "", "operator", "generate-root", "-decode="+encoded, "-otp="+otp)
		newRoot = strings.TrimSpace(bare)
	}
	if newRoot == "" {
		return fmt.Errorf("decode produced no token")
	}

	// Verify it actually works and is root.
	lookupOut, _, err := baoExec(pod, newRoot, "", "token", "lookup", "-format=json")
	if err != nil {
		emitRecoveryToken(newRoot, "self-lookup failed")
		return fmt.Errorf("new root token failed self-lookup")
	}
	if !policiesIncludeRoot(lookupOut) {
		emitRecoveryToken(newRoot, "token verified but not root")
		return fmt.Errorf("new token is valid but not root")
	}
	fmt.Println("New token verified: policies include root.")

	if !o.updateGHA {
		fmt.Printf("\n===================================================================\n")
		fmt.Printf("NEW ROOT TOKEN (save now — not stored anywhere):\n  %s\n", newRoot)
		fmt.Printf("===================================================================\n")
		fmt.Println("\nRe-run with --update-gha-secret [--repo owner/repo] to set it into infra-" + region + ".")
		return nil
	}
	return updateRootGHASecret(region, newRoot, o)
}

// updateRootGHASecret writes OPENBAO_ROOT_TOKEN to infra-<region> and verifies
// the env-level write actually landed (gh can silently fall back to repo-level).
func updateRootGHASecret(region, newRoot string, o regenRootOpts) error {
	f := forgeForFn(o.ghHost, o.repo)
	ctx := bg()
	scope := forge.Env("infra-" + region)

	if err := f.SetSecret(ctx, "OPENBAO_ROOT_TOKEN", newRoot, scope); err != nil {
		emitRecoveryToken(newRoot, "secret set failed (token NOT written)")
		return fmt.Errorf("secret set failed: %w", err)
	}
	// Authoritative env-level check (a forge can silently fall back to repo-level
	// when the env doesn't exist or the token lacks env-admin scope).
	names, _ := f.SecretNames(ctx, scope)
	if !secretListed(names, "OPENBAO_ROOT_TOKEN") {
		emitRecoveryToken(newRoot, "env-secret on infra-"+region+" NOT updated (--env likely ignored; create the env / grant env-admin scope)")
		return fmt.Errorf("OPENBAO_ROOT_TOKEN not present on infra-%s after set — --env was ignored", region)
	}
	sum := sha256Hex(newRoot)
	fmt.Printf("OPENBAO_ROOT_TOKEN written to infra-%s environment secret. sha256=%s\n", region, sum)
	fmt.Printf("\nNext: run bootstrap-openbao.yml for region=%s (preserve_root_on_failure=true), then delete OPENBAO_ROOT_TOKEN from infra-%s once it succeeds.\n", region, region)
	return nil
}

// ── pod exec + raw-mode read ─────────────────────────────────────────────────

// findLeaderPod returns the pod reporting is_self=true (the active raft leader),
// falling back to platform-openbao-0.
func findLeaderPod() string {
	for _, cand := range []string{"platform-openbao-0", "platform-openbao-1", "platform-openbao-2"} {
		out, _, err := baoExec(cand, "", "", "status", "-format=json")
		if err == nil && parseIsSelf(out) {
			return cand
		}
	}
	return "platform-openbao-0"
}

// baoExec runs `bao <args>` inside the openbao container of pod via kubectl exec.
// token (if non-empty) sets VAULT_TOKEN; stdin (if non-empty) is piped in.
func baoExec(pod, token, stdin string, args ...string) (stdout, stderr string, err error) {
	argv := []string{"-n", openbaoNS, "exec", "-i", "-c", "openbao", pod, "--",
		"env", "VAULT_ADDR=https://127.0.0.1:8200", "VAULT_SKIP_VERIFY=true"}
	if token != "" {
		argv = append(argv, "VAULT_TOKEN="+token)
	}
	argv = append(argv, "bao")
	argv = append(argv, args...)
	cmd := exec.Command("kubectl", argv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

// readSecretLine reads one line from the terminal without echoing it.
func readSecretLine() (string, error) {
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	return strings.TrimRight(string(b), "\r\n"), err
}

func emitRecoveryToken(token, reason string) {
	fmt.Fprintf(os.Stderr, "\n==================================================================\n")
	fmt.Fprintf(os.Stderr, "Quorum reached and root token decoded, but %s.\nSave this token NOW (not stored anywhere else):\n  %s\n", reason, token)
	fmt.Fprintf(os.Stderr, "==================================================================\n")
}

// ── pure parse helpers (unit-tested) ─────────────────────────────────────────

func parseBaoStatus(s string) (sealed bool, threshold int) {
	var v struct {
		Sealed bool `json:"sealed"`
		T      int  `json:"t"`
	}
	_ = json.Unmarshal([]byte(s), &v)
	return v.Sealed, v.T
}

func parseIsSelf(s string) bool {
	var v struct {
		IsSelf bool `json:"is_self"`
	}
	_ = json.Unmarshal([]byte(s), &v)
	return v.IsSelf
}

func parseGenRootInit(s string) (nonce, otp string) {
	var v struct {
		Nonce string `json:"nonce"`
		OTP   string `json:"otp"`
	}
	_ = json.Unmarshal([]byte(s), &v)
	return v.Nonce, v.OTP
}

func parseGenRootStep(s string) (complete bool, progress, required int, encoded string) {
	var v struct {
		Complete     bool   `json:"complete"`
		Progress     int    `json:"progress"`
		Required     int    `json:"required"`
		EncodedToken string `json:"encoded_token"`
	}
	_ = json.Unmarshal([]byte(s), &v)
	return v.Complete, v.Progress, v.Required, v.EncodedToken
}

func parseTokenField(s string) string {
	var v struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal([]byte(s), &v)
	return v.Token
}

func policiesIncludeRoot(lookupJSON string) bool {
	var v struct {
		Data struct {
			Policies []string `json:"policies"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(lookupJSON), &v)
	for _, p := range v.Data.Policies {
		if p == "root" {
			return true
		}
	}
	return false
}

// secretListed reports whether the forge's secret-name list contains name.
func secretListed(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
