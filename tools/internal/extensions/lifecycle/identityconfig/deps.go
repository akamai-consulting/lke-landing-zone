package identityconfig

// deps.go — the four edges this package could not bring with it.
//
// The four-file set measured 11 outbound BOTH with and without
// ci_openbao_configure.go. The count did not improve; the SHAPE did. Without that
// file, three of the eleven were `DiscoverManagedDomain`, `keycloakIssuerFor` and
// `SpecTeams` — real couplings to a file staying behind. With it, every edge is
// one of these four, and each is here because it is genuinely cuttable:
//
//   - execOutput/execCombined: kubectlprobe already owns the first and this
//     package needs the second, which is DIAGNOSTICS-ONLY — it ignores exit status
//     so a tool's own error text ("No resources found", a NotFound, a describe's
//     Events block) survives, where execOutput is stdout-only and error-gated.
//   - kubectlOut: two lines over execOutput. Copied.
//   - forgeFromEnv: a one-line adapter over forge.FromEnv. Copied.

import (
	"os/exec"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/forge"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/kubectlprobe"
)

// execOutput is the stdout-only, error-gated exec. It delegates to
// kubectlprobe.Exec through a CLOSURE rather than being assigned its value: a
// direct assignment would snapshot whatever kubectlprobe.Exec pointed at when
// this package initialised, freezing it before any test could swap it. That bug
// has recurred.
func execOutput(name string, args ...string) ([]byte, error) { return kubectlprobe.Exec(name, args...) }

// kubectlOut is execOutput with the binary name fixed and the bytes stringified.
func kubectlOut(args ...string) (string, error) {
	out, err := execOutput("kubectl", args...)
	return string(out), err
}

// execCombined runs name with args and returns their combined stdout+stderr as a
// string, IGNORING EXIT STATUS. Diagnostics-only: on a failure path we want the
// tool's own error text — "No resources found" (an empty namespace), a NotFound,
// a describe's Events block — to surface, all of which execOutput (stdout-only,
// error-gated) would swallow.
var execCombined = func(name string, args ...string) string {
	out, _ := exec.Command(name, args...).CombinedOutput()
	return string(out)
}

// forgeFromEnv resolves the git forge from the ambient environment.
var forgeFromEnv = func() (forge.Forge, error) { return forge.FromEnv() }
