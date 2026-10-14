package openbao

// regenroot.go ports regenerate-openbao-root.sh into `llz openbao regen-root`:
// the standard `bao operator generate-root` quorum flow (3-of-5 unseal-key
// holders) plus an optional re-seed of the infra-<region> OPENBAO_ROOT_TOKEN
// env secret. OpenBao's API is only reachable in-cluster, so — like the bash —
// this drives the bao CLI via `kubectl exec` against the active raft leader; the
// Go wins are that the binary travels with the operator (no instance-scripts/
// checkout needed) and the unseal keys are read in terminal raw mode (never
// echoed, never on argv, never on disk).

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/baoread"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/health"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/openbao"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/s3sig"
)

type RegenRootOpts struct {
	UpdateGHA bool
	Repo      string
}

func RunRegenRoot(dryRun bool, region string, o RegenRootOpts) error {
	if region == "" {
		return fmt.Errorf("usage: llz openbao regen-root <region> [--update-gha-secret] [--repo owner/repo]")
	}
	// DRY-RUN BEFORE ANY EXEC. findLeaderPod probes three pods, and now that this
	// file goes through the RESILIENT exec each probe carries the 24-try transient
	// budget — so under a konnectivity outage a --dry-run sat silent for ~16
	// minutes before printing its first line and returning. A dry run must not
	// touch the cluster at all.
	if dryRun {
		fmt.Fprintln(os.Stderr, "→ (dry-run) would resolve the raft leader and run the bao generate-root quorum flow against it")
		return nil
	}
	pod := findLeaderPod()
	ctx, _ := kubectlprobe.Exec("kubectl", "config", "current-context")
	fmt.Printf("kubectl context: %s\n", strings.TrimSpace(string(ctx)))
	fmt.Printf("Target pod:      %s/%s (active raft leader)\n", baoread.Namespace, pod)
	fmt.Printf("Region (for GHA env name only): %s\n\n", region)

	// Sanity: reachable + unsealed.
	//
	// PARSE STDOUT REGARDLESS OF THE EXEC ERROR — the same rule this PR applies to
	// healthsla's baoStatus, and the same one baoread.ParsePodStatus's doc states.
	// `bao status` EXITS 2 WHEN SEALED and still prints valid JSON, so returning on
	// err made the `if sealed` branch below dead in production: an operator with a
	// sealed cluster was told "cannot reach OpenBao … via the current kubectl
	// context" and sent to check their kubeconfig, which was fine. The test stubbed
	// (json, nil) — a combination the real CLI never emits — which is why it did
	// not show.
	statusOut, _, err := baoread.ExecFn(pod, "", "", "status", "-format=json")
	sealed, threshold, parsed := openbao.ParseStatusOK(statusOut)
	if !parsed {
		return fmt.Errorf("cannot read OpenBao's seal state at %s/%s: no usable JSON from `bao status` (%v). "+
			"A SEALED pod exits non-zero and still prints JSON, so this means the exec did not reach a "+
			"running bao — check the pod is Ready and the kubectl context is right",
			baoread.Namespace, pod, err)
	}
	if sealed {
		return fmt.Errorf("%s is sealed — unseal it first, then re-run", pod)
	}
	fmt.Printf("OpenBao unsealed. Unseal threshold: %d.\n", threshold)

	// Clean slate, then init.
	_, _, _ = baoread.ExecFn(pod, "", "", "operator", "generate-root", "-cancel")
	initOut, _, err := baoread.ExecFn(pod, "", "", "operator", "generate-root", "-init", "-format=json")
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
		out, errOut, err := baoread.ExecFn(pod, "", key+"\n",
			"operator", "generate-root", "-nonce="+nonce, "-format=json", "-")
		key = ""
		if err != nil {
			_, _, _ = baoread.ExecFn(pod, "", "", "operator", "generate-root", "-cancel")
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
	decodeOut, _, _ := baoread.ExecFn(pod, "", "", "operator", "generate-root", "-decode="+encoded, "-otp="+otp, "-format=json")
	newRoot := parseTokenField(decodeOut)
	if newRoot == "" { // older bao prints a bare token
		bare, _, _ := baoread.ExecFn(pod, "", "", "operator", "generate-root", "-decode="+encoded, "-otp="+otp)
		newRoot = strings.TrimSpace(bare)
	}
	if newRoot == "" {
		return fmt.Errorf("decode produced no token")
	}

	// Verify it actually works and is root.
	lookupOut, _, err := baoread.ExecFn(pod, newRoot, "", "token", "lookup", "-format=json")
	if err != nil {
		emitRecoveryToken(newRoot, "self-lookup failed")
		return fmt.Errorf("new root token failed self-lookup")
	}
	if !openbao.PoliciesIncludeRoot(lookupOut) {
		emitRecoveryToken(newRoot, "token verified but not root")
		return fmt.Errorf("new token is valid but not root")
	}
	fmt.Println("New token verified: policies include root.")

	if !o.UpdateGHA {
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
func updateRootGHASecret(region, newRoot string, o RegenRootOpts) error {
	if _, err := execLookPath("gh"); err != nil {
		emitRecoveryToken(newRoot, "gh CLI not installed")
		return fmt.Errorf("gh not installed but --update-gha-secret was requested")
	}
	repoArgs := []string{}
	if o.Repo != "" {
		repoArgs = []string{"--repo", o.Repo}
	}

	set := exec.Command("gh", append([]string{"secret", "set", "OPENBAO_ROOT_TOKEN", "--env", "infra-" + region}, repoArgs...)...)
	set.Stdin = strings.NewReader(newRoot)
	if out, err := set.CombinedOutput(); err != nil {
		emitRecoveryToken(newRoot, "gh secret set failed (token NOT written)")
		return fmt.Errorf("gh secret set failed: %s", strings.TrimSpace(string(out)))
	}
	// Authoritative env-level check (gh's success message is version-dependent).
	list := exec.Command("gh", append([]string{"secret", "list", "--env", "infra-" + region}, repoArgs...)...)
	out, _ := list.Output()
	if !secretListed(string(out), "OPENBAO_ROOT_TOKEN") {
		emitRecoveryToken(newRoot, "env-secret on infra-"+region+" NOT updated (--env likely ignored; create the env / grant env-admin scope)")
		return fmt.Errorf("OPENBAO_ROOT_TOKEN not present on infra-%s after set — --env was ignored", region)
	}
	sum := s3sig.SHA256Hex(newRoot)
	fmt.Printf("OPENBAO_ROOT_TOKEN written to infra-%s environment secret. sha256=%s\n", region, sum)
	fmt.Printf("\nNext: run bootstrap-openbao.yml for region=%s (preserve_root_on_failure=true), then delete OPENBAO_ROOT_TOKEN from infra-%s once it succeeds.\n", region, region)
	return nil
}

// ── pod exec + raw-mode read ─────────────────────────────────────────────────

// resolveGenerateRootPod returns the pod that will ACCEPT the generate-root
// flow, and whether one was found at all.
//
// WHY THIS EXISTS AS A SHARED HELPER. `sys/generate-root/*` is unauthenticated
// and node-local: a raft standby does not forward it, it rejects it outright
// with `400 * Vault is in standby mode`. Authenticated calls (`token lookup`,
// `token revoke -self`) ARE forwarded, so a standby answers those happily —
// which is why aiming at the wrong pod stays invisible until the quorum flow
// starts, several steps in. Pod ordinal has nothing to do with raft leadership;
// platform-openbao-0 is the leader only until the first failover or restart.
//
// A single non-HA node reports neither is_self nor ha_enabled and is a valid
// target — there is no standby to be rejected by — so `standalone` counts as
// found. Only "every pod answered, all of them standbys" (or nothing answered)
// is a miss.
func resolveGenerateRootPod() (string, bool) {
	// PARSE STDOUT REGARDLESS OF THE EXEC ERROR. `bao status` EXITS 2 WHEN SEALED
	// and still prints valid JSON, so gating on err skips every pod of a sealed
	// cluster and reports "no leader" — sending the operator to hunt an election
	// problem when the real answer is "unsealed it". A sealed pod publishes neither
	// is_self nor ha_enabled, so it parses as standalone and is returned here; the
	// caller's seal check is what turns it into the accurate message. Unparseable
	// output (pod unreachable, exec failed) is the only reason to skip a candidate.
	for _, cand := range baoread.PodNames {
		out, _, _ := baoread.ExecFn(cand, "", "", "status", "-format=json")
		if st, err := health.ParseBaoStatus([]byte(out)); err == nil && st.HAMode != "standby" {
			return cand, true
		}
	}
	return "", false
}

// findLeaderPod resolves the generate-root target for the INTERACTIVE flow,
// keeping its long-standing fall back to platform-openbao-0 when no node claims
// to be active — an operator at a terminal can read the resulting error and
// judge it. RunRegenRootCI deliberately does not fall back: unattended, a guess
// that lands on a standby just fails later and less legibly.
func findLeaderPod() string {
	if pod, ok := resolveGenerateRootPod(); ok {
		return pod
	}
	return baoread.PodNames[0]
}

// readSecretLine is a package var so a test can reach the quorum loop. Without a
// seam here the interactive read blocks and the loop this file's retry fix is
// about is unreachable — which is exactly why its first gate stopped at the
// sealed check and passed with the fix reverted.
var readSecretLine = readSecretLineFromTerminal

func readSecretLineFromTerminal() (string, error) {
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	return strings.TrimRight(string(b), "\r\n"), err
}

func emitRecoveryToken(token, reason string) {
	fmt.Fprintf(os.Stderr, "\n==================================================================\n")
	fmt.Fprintf(os.Stderr, "Quorum reached and root token decoded, but %s.\nSave this token NOW (not stored anywhere else):\n  %s\n", reason, token)
	fmt.Fprintf(os.Stderr, "==================================================================\n")
}

// ── pure parse helpers (unit-tested) ─────────────────────────────────────────

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

// secretListed reports whether `gh secret list` output contains name in column 1.
func secretListed(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == name {
			return true
		}
	}
	return false
}
