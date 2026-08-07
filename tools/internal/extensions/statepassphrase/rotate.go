package statepassphrase

// ci_rotate_state_passphrase.go implements `llz ci rotate-state-passphrase` — the
// rollover half of OpenTofu state-encryption key rotation (ADR 0007 (state encryption) / ADR 0009).
//
// WHY A ROLLOVER IS POSSIBLE AT ALL. Three facts, each verified against OpenTofu
// 1.12.3 rather than assumed:
//
//  1. `enforced = true` bans method.unencrypted — NOT `fallback`. An ENCRYPTED
//     fallback is legal alongside it, so a rollover never relaxes the posture.
//     (The roots' encryption.tf comment says the two are "mutually exclusive";
//     that is true only of the unencrypted fallback.)
//
//  2. The key-provider NAME is decryption metadata. pbkdf2 stores its salt at
//     meta["key_provider.pbkdf2.<name>"], so a passphrase can only decrypt state
//     written under the SAME name. Redefining `llz` to hold a new passphrase
//     feeds the old salt to the new key and fails with "decryption failed for all
//     attempted". Rotation therefore adds a NEW name (TF_STATE_ENCRYPTION_KEY_NAME)
//     and keeps the old one as the fallback (…_KEY_NAME_OLD).
//
//  3. `tofu state pull | tofu state push -` re-encrypts under the current primary
//     with NO provider API calls, and the plaintext never touches disk. That is
//     the re-key: cheaper than `apply -refresh-only`, and it cannot be broken by
//     a provider outage.
//
// SAFETY. The old passphrase is the only thing that can read a root that has not
// rolled over yet, so this command never deletes it — it re-keys, then VERIFIES
// every root decrypts with the new key ALONE, and reports. Discarding
// TF_STATE_ENCRYPTION_PASSPHRASE_OLD is a separate, gated step that must not run
// unless every root verified. A partially-rolled instance is fully recoverable
// precisely because both keys are still configured; re-running converges.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/ghaout"
)

// statePassphraseRoots are the Terraform roots whose state is encrypted. Kept
// explicit rather than globbed: a root missing from this list is silently never
// re-keyed, which is exactly the failure that strands a state file on a
// passphrase nobody holds any more.
var statePassphraseRoots = []string{"vpc", "cluster", "object-storage", "databases"}

// rootRollover is one root's outcome.
type rootRollover struct {
	Root     string
	Rekeyed  bool
	Verified bool // decrypts with the NEW passphrase alone
	Skipped  string
	Err      string
}

// Seams for tests.
//
// STDOUT OF `tofu state pull` IS PLAINTEXT STATE — kubeconfigs, database admin
// passwords, the whole reason ADR 0007 (state encryption) exists. Neither seam captures it: the
// re-key pipes it straight into `state push` inside one shell so it never lands
// in a Go buffer or on disk, and the verify discards it outright. Only STDERR is
// captured, so a failure message can never carry decrypted state into a log, a
// job summary, or an error string.
var (
	// rekeyStateFn re-encrypts one root's state under the current primary key.
	// The pipe is the point: plaintext exists only in the kernel buffer between
	// the two processes.
	rekeyStateFn = func(dir string) error {
		return runQuiet(dir, nil, "sh", "-c", "tofu state pull | tofu state push -")
	}
	// verifyStateFn proves the root decrypts with the new passphrase ALONE — the
	// step that licenses discarding the old one. Reading it back is the proof;
	// the content is irrelevant and thrown away.
	verifyStateFn = func(dir, encWithNewKeyOnly string) error {
		return runQuiet(dir, []string{"TF_ENCRYPTION=" + encWithNewKeyOnly}, "tofu", "state", "pull")
	}
	statePassphraseRootExists = func(dir string) bool {
		st, err := os.Stat(dir)
		return err == nil && st.IsDir()
	}
)

// runQuiet runs a command in dir with stdout DISCARDED and only stderr captured
// for the error message. extraEnv entries override the inherited environment.
func runQuiet(dir string, extraEnv []string, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Dir = dir
	if len(extraEnv) > 0 {
		c.Env = append(os.Environ(), extraEnv...)
	}
	var stderr bytes.Buffer
	c.Stdout = io.Discard
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", lastLines(msg, 3))
	}
	return nil
}

// lastLines trims a tool's error to its final n lines — enough to diagnose,
// short enough not to bury a job summary in a stack of OpenTofu banner output.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// buildNewKeyOnlyEncryption renders a TF_ENCRYPTION config carrying ONLY the new
// key — no fallback. Verifying with the rotation-window config would decrypt via
// the fallback and pass for a root still on the OLD key, so this is what makes
// "verified" mean "the old passphrase can now be deleted".
//
// The passphrase is interpolated into an HCL string, so it is validated to the
// same base64 alphabet the terraform-init action enforces: a quote or backslash
// could close the string and append arbitrary encryption configuration (e.g.
// swapping in method.unencrypted). The key name becomes an HCL identifier.
func buildNewKeyOnlyEncryption(passphrase, keyName string) (string, error) {
	if strings.TrimSpace(passphrase) == "" {
		return "", fmt.Errorf("TF_STATE_ENCRYPTION_PASSPHRASE is not set — the verify pass needs the new passphrase to prove each root decrypts WITHOUT the fallback")
	}
	if keyName == "" {
		keyName = "llz"
	}
	if strings.ContainsFunc(passphrase, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '+' || r == '/' || r == '=' || r == '_' || r == '-')
	}) {
		return "", fmt.Errorf("TF_STATE_ENCRYPTION_PASSPHRASE must contain only [A-Za-z0-9+/=_-] — it is interpolated into an HCL string where a quote or backslash could inject encryption configuration")
	}
	if strings.ContainsFunc(keyName, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
	}) {
		return "", fmt.Errorf("TF_STATE_ENCRYPTION_KEY_NAME %q must be an HCL identifier: [A-Za-z0-9_] only", keyName)
	}
	return fmt.Sprintf(`key_provider "pbkdf2" %[1]q {
  passphrase = %[2]q
}
method "aes_gcm" %[1]q {
  keys = key_provider.pbkdf2.%[1]s
}
method "unencrypted" "migrate" {}
state {
  method = method.aes_gcm.%[1]s
}
plan {
  method = method.aes_gcm.%[1]s
}
`, keyName, passphrase), nil
}

func RunRotate(apply bool, rootsDir string) error {
	encBoth := os.Getenv("TF_ENCRYPTION")
	if strings.TrimSpace(encBoth) == "" {
		return fmt.Errorf("TF_ENCRYPTION is not set — run this inside a job that used the terraform-init action")
	}
	// The rollover is only meaningful when a fallback is configured: with a single
	// key, `state push` re-writes under the same key and the verify is vacuous.
	if !strings.Contains(encBoth, "fallback") {
		return fmt.Errorf("TF_ENCRYPTION carries no fallback — this is not a rotation window. " +
			"Set TF_STATE_ENCRYPTION_PASSPHRASE_OLD (+ TF_STATE_ENCRYPTION_KEY_NAME_OLD) so terraform-init emits one")
	}

	// Built here rather than passed in: the verify pass needs the NEW key ALONE,
	// and emitting that HCL in workflow YAML both duplicated the terraform-init
	// action's escaping logic and put untested shell on the critical path of a
	// state re-key. One implementation, unit-tested.
	encNewOnly, err := buildNewKeyOnlyEncryption(
		os.Getenv("TF_STATE_ENCRYPTION_PASSPHRASE"), os.Getenv("TF_STATE_ENCRYPTION_KEY_NAME"))
	if err != nil {
		return err
	}

	results := make([]rootRollover, 0, len(statePassphraseRoots))
	for _, root := range statePassphraseRoots {
		dir := rootsDir + "/" + root
		r := rootRollover{Root: root}
		if !statePassphraseRootExists(dir) {
			r.Skipped = "root not present in this instance"
			results = append(results, r)
			continue
		}
		if !apply {
			results = append(results, r)
			continue
		}
		if err := rekeyStateFn(dir); err != nil {
			r.Err = "re-key: " + err.Error()
			results = append(results, r)
			continue
		}
		r.Rekeyed = true
		if err := verifyStateFn(dir, encNewOnly); err != nil {
			r.Err = "verify with the new key alone: " + err.Error()
			results = append(results, r)
			continue
		}
		r.Verified = true
		results = append(results, r)
	}

	sort.SliceStable(results, func(i, j int) bool { return results[i].Root < results[j].Root })
	return reportRollover(results, apply)
}

// reportRollover prints the per-root outcome and fails the command unless every
// present root verified. The exit status is what the workflow gates the
// old-secret deletion on, so it must be false only when discarding is safe.
func reportRollover(results []rootRollover, apply bool) error {
	var lines []string
	var failed, verified, skipped int
	for _, r := range results {
		switch {
		case r.Skipped != "":
			skipped++
			lines = append(lines, fmt.Sprintf("  - `%s` — skipped (%s)", r.Root, r.Skipped))
		case r.Err != "":
			failed++
			lines = append(lines, fmt.Sprintf("  - `%s` — **FAILED**: %s", r.Root, r.Err))
		case r.Verified:
			verified++
			lines = append(lines, fmt.Sprintf("  - `%s` — re-keyed and verified with the new passphrase alone", r.Root))
		default:
			lines = append(lines, fmt.Sprintf("  - `%s` — would be re-keyed (dry run)", r.Root))
		}
	}
	for _, l := range lines {
		fmt.Println(strings.TrimPrefix(l, "  - "))
	}

	if !apply {
		fmt.Println("\nDry run — nothing was re-keyed. Re-run with --apply inside the rotation window.")
		return ghaout.Append("GITHUB_STEP_SUMMARY", append([]string{"### State-passphrase rollover (dry run)", ""}, lines...)...)
	}

	summary := append([]string{"### State-passphrase rollover", ""}, lines...)
	if failed > 0 {
		summary = append(summary, "",
			"**DO NOT delete `TF_STATE_ENCRYPTION_PASSPHRASE_OLD`.** "+
				fmt.Sprintf("%d root(s) did not verify; the old passphrase is the only thing that can still read them. ", failed)+
				"Fix the failure and re-run — the rollover is idempotent.")
		_ = ghaout.Append("GITHUB_STEP_SUMMARY", summary...)
		return fmt.Errorf("state-passphrase rollover incomplete: %d of %d root(s) failed — old passphrase MUST be retained", failed, verified+failed)
	}
	summary = append(summary, "",
		fmt.Sprintf("All %d present root(s) verified with the new passphrase alone (%d skipped). ", verified, skipped)+
			"`TF_STATE_ENCRYPTION_PASSPHRASE_OLD` can now be deleted and "+
			"`TF_STATE_ENCRYPTION_KEY_NAME_OLD` cleared.")
	return ghaout.Append("GITHUB_STEP_SUMMARY", summary...)
}
