package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// colorEnvProbe marks a child process spawned by
// TestColorOnHonoursTheEnvOptOuts: "on"/"off" is the colorOn value the parent
// expects for the env it handed down.
const colorEnvProbe = "LLZ_COLOR_ENV_PROBE"

// colorOn is computed ONCE, at package-variable initialization, from the
// process environment — so the opt-outs cannot be exercised by mutating env
// inside a test. Re-exec this test binary with a controlled environment
// instead; the child asserts, the parent reports.
//
// The three cases pin the precedence the doc comment claims: NO_COLOR and
// TERM=dumb are hard opt-outs that beat CLICOLOR_FORCE, and neither of them
// fires when it is absent (a child's stdout is a pipe, so CLICOLOR_FORCE is the
// only thing that can turn color ON).
func TestColorOnHonoursTheEnvOptOuts(t *testing.T) {
	if want, isChild := os.LookupEnv(colorEnvProbe); isChild {
		if got := colorOn; got != (want == "on") {
			t.Fatalf("colorOn = %v, want %v (NO_COLOR=%q TERM=%q CLICOLOR_FORCE=%q)",
				got, want == "on", os.Getenv("NO_COLOR"), os.Getenv("TERM"), os.Getenv("CLICOLOR_FORCE"))
		}
		if painted := red("x") != "x"; painted != (want == "on") {
			t.Fatalf("red() painted=%v, want %v", painted, want == "on")
		}
		return
	}

	for _, tc := range []struct {
		name, want string
		env        []string
	}{
		{"CLICOLOR_FORCE turns color on", "on", []string{"NO_COLOR=", "TERM=xterm-256color", "CLICOLOR_FORCE=1"}},
		{"NO_COLOR beats CLICOLOR_FORCE", "off", []string{"NO_COLOR=1", "TERM=xterm-256color", "CLICOLOR_FORCE=1"}},
		{"TERM=dumb beats CLICOLOR_FORCE", "off", []string{"NO_COLOR=", "TERM=dumb", "CLICOLOR_FORCE=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestColorOnHonoursTheEnvOptOuts$", "-test.v")
			cmd.Env = append(append(os.Environ(), tc.env...), colorEnvProbe+"="+tc.want)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child (%s) failed: %v\n%s", strings.Join(tc.env, " "), err, out)
			}
		})
	}
}
