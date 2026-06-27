package main

// fetchkubeconfig_state.go implements `llz ci fetch-kubeconfig-state` — the
// native port of the fetch-kubeconfig composite action's inline terraform
// init + output extraction. The TF-state-sourced sibling of `llz ci
// fetch-kubeconfig` (the Linode-API path): consumers that must read the exact
// kubeconfig Terraform manages — e.g. right after an lke-admin rotation's
// refresh-only repopulated kubeconfig_raw — go through state, not the API.
//
// Runs from the cluster terraform working directory (the composite action sets
// working-directory). When kubeconfig_raw comes back empty, the diagnostics
// distinguish the classic failure modes: a failed/odd init reading a DIFFERENT
// (empty) state than cluster-bootstrap did, vs the output genuinely absent.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func ciFetchKubeconfigStateCmd() *cobra.Command {
	var region, output string
	var allowMissing bool
	c := &cobra.Command{
		Use:   "fetch-kubeconfig-state",
		Short: "init the cluster TF backend and write the kubeconfig_raw output to a file",
		Long: "Native port of the fetch-kubeconfig composite action's inline body. Runs\n" +
			"`terraform init` against the cluster/<region> state key (bucket from\n" +
			"$TF_STATE_BUCKET; init output is NOT swallowed — a failed init is the top\n" +
			"suspect when `terraform output` reads empty against a state that has the\n" +
			"value), then writes `terraform output -raw kubeconfig_raw` to --output\n" +
			"(mode 0600). An empty output dumps grouped diagnostics (output keys, state\n" +
			"resources) and either fails or, with --allow-missing, sets available=false\n" +
			"on GITHUB_OUTPUT. Run from the cluster terraform working directory.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runCIFetchKubeconfigState(region, output, allowMissing)
		},
	}
	c.Flags().StringVar(&region, "region", "", "deployment/env key, e.g. primary (required)")
	c.Flags().StringVar(&output, "output", "", "absolute path to write the kubeconfig to (required)")
	c.Flags().BoolVar(&allowMissing, "allow-missing", false, "set available=false instead of failing when kubeconfig_raw is absent")
	return c
}

// tfInitStream runs `terraform init` with streamed output. A package var so
// tests can record the backend config without a real backend.
var tfInitStream = func(args ...string) error {
	cmd := exec.Command("terraform", append([]string{"init"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// tfInitAttempts / tfInitBackoff / tfInitSleep bound the retry around terraform
// init below. Package vars so tests can neutralize the wait.
var (
	tfInitAttempts = 3
	tfInitBackoff  = func(attempt int) time.Duration { return time.Duration(attempt) * 5 * time.Second }
	tfInitSleep    = time.Sleep
)

// tfInitWithRetry runs `terraform init`, retrying a few times on failure.
// init pulls providers from the registry, modules over git, and reaches the S3
// state backend — all transient-prone on a hosted runner — and is idempotent,
// so a single blip must not fail a long (~30-min) provisioning run on the first
// attempt.
func tfInitWithRetry(args ...string) error {
	var err error
	for attempt := 1; attempt <= tfInitAttempts; attempt++ {
		if err = tfInitStream(args...); err == nil {
			return nil
		}
		if attempt < tfInitAttempts {
			fmt.Fprintf(os.Stderr, "llz: terraform init failed (attempt %d/%d), retrying: %v\n",
				attempt, tfInitAttempts, err)
			tfInitSleep(tfInitBackoff(attempt))
		}
	}
	return err
}

func runCIFetchKubeconfigState(region, output string, allowMissing bool) error {
	if region == "" || output == "" {
		return fmt.Errorf("--region and --output are required")
	}
	bucket := os.Getenv("TF_STATE_BUCKET")
	if bucket == "" {
		return fmt.Errorf("TF_STATE_BUCKET must be set (the S3 state bucket)")
	}
	stateKey := fmt.Sprintf("cluster/%s/terraform.tfstate", region)

	if err := tfInitWithRetry(
		"-backend-config=bucket="+bucket,
		"-backend-config=key="+stateKey,
		"-backend-config=region=us-east-1"); err != nil {
		return fmt.Errorf("terraform init failed for %s (bucket=%s) after %d attempts", stateKey, bucket, tfInitAttempts)
	}

	if dir := filepath.Dir(output); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	// Capture stderr (don't discard it) so an empty result is explainable.
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("terraform", "output", "-raw", "kubeconfig_raw")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	_ = cmd.Run()

	if stdout.Len() == 0 {
		fetchKubeconfigStateDiagnostics(region, stateKey, bucket, stderr.String())
		if allowMissing {
			fmt.Fprintf(os.Stderr, "::warning::kubeconfig_raw for %s is absent or empty (allow-missing=true) — see diagnostics above.\n", region)
			setGHAOutput("available", "false")
			return nil
		}
		return fmt.Errorf("kubeconfig_raw for %s is empty. cluster-bootstrap reads the SAME state output via terraform_remote_state; if that job passed in this build the state HAS this output — the diagnostics above show why 'terraform output -raw' did not", region)
	}

	if err := os.WriteFile(output, stdout.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing kubeconfig to %s: %w", output, err)
	}
	setGHAOutput("available", "true")
	return nil
}

// fetchKubeconfigStateDiagnostics dumps why kubeconfig_raw read empty —
// best-effort, grouped for the run log.
func fetchKubeconfigStateDiagnostics(region, stateKey, bucket, outputStderr string) {
	fmt.Printf("::group::fetch-kubeconfig diagnostics — kubeconfig_raw empty for %s\n", region)
	fmt.Printf("state key : %s\n", stateKey)
	fmt.Printf("bucket    : %s\n", bucket)
	if out, err := execOutput("terraform", "version"); err == nil {
		lines := strings.SplitN(strings.TrimSpace(string(out)), "\n", 3)
		for _, l := range lines[:min(2, len(lines))] {
			fmt.Println(l)
		}
	}
	fmt.Println("--- terraform output -raw kubeconfig_raw stderr ---")
	if strings.TrimSpace(outputStderr) == "" {
		fmt.Println("(no stderr captured)")
	} else {
		fmt.Println(strings.TrimSpace(outputStderr))
	}
	fmt.Println("--- root output keys (json) ---")
	keysListed := false
	if out, err := execOutput("terraform", "output", "-json"); err == nil {
		var outputs map[string]json.RawMessage
		if json.Unmarshal(out, &outputs) == nil {
			for k := range outputs {
				fmt.Println(k)
				keysListed = true
			}
		}
	}
	if !keysListed {
		fmt.Println("(could not enumerate output keys)")
	}
	fmt.Println("--- state resources (lke / kubeconfig) ---")
	matched := false
	if out, err := execOutput("terraform", "state", "list"); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			lower := strings.ToLower(l)
			if strings.Contains(lower, "lke") || strings.Contains(lower, "kubeconfig") {
				fmt.Println(l)
				matched = true
			}
		}
	}
	if !matched {
		fmt.Println("(no matching resources in state — terraform output is reading a DIFFERENT/empty state than cluster-bootstrap did)")
	}
	fmt.Println("::endgroup::")
}
