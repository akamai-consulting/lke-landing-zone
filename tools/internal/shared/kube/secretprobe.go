package kube

// secretprobe.go — kubectl-shaped Secret helpers, moved out of ci_bao_seed.go.
//
// THEY WERE NEVER THE SEEDER'S. `k8sSecretField` and `describeSecretForDiag` were
// defined in the OpenBao seeding verb and called from users.go, ci_assertidentity.go
// and ci_keycloak_configure.go — four callers, none of which seeds anything. They
// read a Kubernetes Secret and explain why one is missing, which is what this
// package is for; SecretField (the `kubectl get -o json` decoder) already lives
// here for the same reason.
//
// DescribeSecret is a DIAGNOSTIC and deliberately never fails: on managed vs
// self-install the platform's Secrets differ by name, and that rename is exactly
// what broke keycloak-configure once. Naming the alternatives is the whole value.

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cigate"
)

// Exec captures a command's stdout. A package var so tests drive these helpers
// without a cluster; internal/cli installs the real one.
var Exec = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Combined runs a command and returns its combined output plus success. A second
// seam because DescribeSecret needs STDERR — it explains why a Secret is missing,
// and kubectl says that on stderr.
//
// IT USED TO CALL exec.Command DIRECTLY, which is why it measured 0% coverage: a
// diagnostic that cannot be driven from a test is a diagnostic nobody has ever
// seen the output of. Routing it through a seam was the fix for both.
var Combined = func(name string, args ...string) (string, bool) {
	return cigate.RunCombined(exec.Command(name, args...))
}

// k8sSecretField reads one key of a K8s Secret, "" on any failure (absent
// Secret/key, bad base64) — the bash `kubectl get secret … -o jsonpath …
// 2>/dev/null | base64 -d || true`.
func SecretFieldOf(ns, name, key string) string {
	out, err := Exec("kubectl", "-n", ns, "get", "secret", name,
		"-o", "jsonpath={.data."+strings.ReplaceAll(key, ".", `\.`)+"}")
	if err != nil {
		return ""
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return ""
	}
	return string(dec)
}

// describeSecretForDiag explains WHY k8sSecretField came back empty, for error
// messages. k8sSecretField deliberately collapses every failure to "" — missing
// namespace, missing Secret, renamed key, no cluster access all look identical —
// so a caller reporting "creds not readable" leaves the reader with nowhere to go.
// This distinguishes them.
//
// It prints KEY NAMES ONLY, never values. The whole point is to be safe to emit
// into a CI log on the failure path, so it must stay that way: `-o jsonpath` over
// `.data` keys, never over `.data.*` contents.
// describeSecretForDiag explains WHY k8sSecretField came back empty, for error
// messages. k8sSecretField deliberately collapses every failure to "" — missing
// namespace, missing Secret, renamed key, no cluster access all look identical —
// so a caller reporting "creds not readable" leaves the reader with nowhere to go.
// This distinguishes them.
//
// It prints KEY NAMES ONLY, never values. The whole point is to be safe to emit
// into a CI log on the failure path, so it must stay that way: `-o jsonpath` over
// `.data` keys, never over `.data.*` contents.
func DescribeSecret(ns, name string) string {
	if out, ok := Combined("kubectl", "get", "namespace", ns); !ok {
		return fmt.Sprintf("namespace %q is not readable (%s) — cluster access or RBAC, not the Secret",
			ns, cigate.FirstLine(out))
	}
	out, ok := Combined("kubectl", "-n", ns, "get", "secret", name,
		"-o", `jsonpath={range $k, $v := .data}{$k}{" "}{end}`)
	if !ok {
		// Name the alternatives: on managed vs self-install these Secrets differ, and
		// that rename is exactly what broke keycloak-configure before.
		names, _ := Combined("kubectl", "-n", ns, "get", "secrets",
			"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
		return fmt.Sprintf("Secret %s/%s does not exist (%s). Secrets present in %s: %s",
			ns, name, cigate.FirstLine(out), ns, strings.Join(nonEmptyFields(names), ", "))
	}
	keys := nonEmptyFields(out)
	if len(keys) == 0 {
		return fmt.Sprintf("Secret %s/%s exists but has NO data keys", ns, name)
	}
	return fmt.Sprintf("Secret %s/%s exists; its keys are: %s", ns, name, strings.Join(keys, ", "))
}

// nonEmptyFields splits on whitespace, dropping empties.
// nonEmptyFields splits on whitespace, dropping empties.
func nonEmptyFields(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// maskGHALines registers a possibly-multiline secret with ::add-mask:: —
// one directive per line, since Actions masking is line-oriented.
