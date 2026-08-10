package cigate

// Package cigate holds the primitives the ci gate verbs share: the kubectl/clock
// seam type and constructor (Deps), the deadline poll loop every gate spells the
// same way, the env-with-default reader, and the kubeconfig tempfile spill. Each
// of these had drifted into three-to-five near-identical inline copies across the
// verbs; collapsing them keeps the incident history (the comments below) in
// exactly one place.
//
// TWELVE non-test callers, which is why it is a package rather than a file in one
// extension: `converge` needs it most, but so do the firewall verbs, the openbao
// gates and the readiness lanes. Extracted with `converge` for the same reason
// guardwalk came out with guard-charts — the extraction that needs it most is the
// one that proves it is shared.
//
// readRegionTFVars and ciClient did NOT come along. They know the repo's
// terraform layout and the CI PAT reader respectively, neither of which is a gate
// primitive; they stayed in internal/cli.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/capability"
	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/extension"
)

// Runner runs one kubectl invocation (KUBECONFIG already wired by the
// caller) and returns its combined output plus whether it exited 0.
type Runner func(args ...string) (string, bool)

// Deps are the seams every ci gate drives: one kubectl invocation plus
// now/sleep for the testable deadline loop. One type for all the gates — the
// kyverno and destroy-unwedge verbs each used to declare their own structurally
// identical copy, to the point that one call site had to write
// `kyvernoDeps(NewDepsFor(…))` to convert between them.
type Deps struct {
	// Kubectl READS. It was a general runner — any argv, including `delete` and
	// `exec` — shared by five of the eight extensions in this tree that mutate a
	// cluster, which meant one seam handed the same power to a read-only assertion
	// and to a deliberate fault injector.
	//
	// It stays a Runner rather than becoming capability.Cluster because these five
	// packages thread it through dozens of helpers by value; the READ half is
	// unchanged and the WRITE half moved to Writer below, which is where the
	// enforcement lives.
	Kubectl Runner
	// Writer is the six named mutations, scoped by the binding's declared grants.
	// A gate that declared no cluster-write receives one that refuses, so the
	// mutation cannot be assembled as an argv and slipped past the declaration.
	Writer capability.Writer
	Now    func() time.Time
	Sleep  func(time.Duration)
}

// Kubectl builds the kubectl runner for the gate seams. kubeconfig == ""
// inherits the ambient environment so kubectl resolves $KUBECONFIG /
// ~/.kube/config itself; a non-empty path pins KUBECONFIG to that file.
//
// RunCombined runs the command BEFORE reading its buffer. The original
// `return buf.String(), c.Run() == nil` evaluated buf.String() first (Go's
// left-to-right operand order), snapshotting the buffer EMPTY before the command
// ever ran — so every gate read back blank output and sat reading sync= health=
// forever. Do not "simplify" this back into a single return expression.
func Kubectl(kubeconfig string) Runner {
	return func(args ...string) (string, bool) {
		c := exec.Command("kubectl", args...)
		if kubeconfig == "" {
			c.Env = os.Environ()
		} else {
			c.Env = EnvWithKubeconfig(kubeconfig)
		}
		return RunCombined(c)
	}
}

// EnvWithKubeconfig returns the process env with KUBECONFIG set to exactly `path`
// — dropping any inherited KUBECONFIG first so kubectl/helm can't read a duplicate
// (often empty) entry instead. Duplicate KUBECONFIG env keys are resolved
// inconsistently, which is how an empty placeholder $KUBECONFIG shadowed the real
// resolved path in the e2e.
//
// Do not "simplify" this to `append(os.Environ(), "KUBECONFIG="+path)` — that is
// the spelling with the bug, and it is what Kubectl above used until this
// helper (written for the bootstrap-cluster runners after that e2e failure) was
// lifted here and shared.
func EnvWithKubeconfig(path string) []string {
	src := os.Environ()
	env := make([]string, 0, len(src)+1)
	for _, e := range src {
		if strings.HasPrefix(e, "KUBECONFIG=") {
			continue
		}
		env = append(env, e)
	}
	return append(env, "KUBECONFIG="+path)
}

// NewDeps builds the real kubectl/clock seam against the ambient
// KUBECONFIG. Package var so tests can swap the whole seam out.
var NewDeps = func() Deps { return NewDepsFor("") }

// NewDepsFor is NewDeps with KUBECONFIG pinned to kubeconfig (the
// tempfile the KUBECONFIG_RAW-driven verbs spill).
func NewDepsFor(kubeconfig string) Deps {
	return Deps{
		Kubectl: Kubectl(kubeconfig),
		// DENIED BY DEFAULT. A gate that mutates has to say so at its construction
		// site via GrantedBy, which makes granting greppable: `grep -rn GrantedBy`
		// lists every place in the tree that hands out cluster-write.
		Writer: capability.For(extension.Binding{}).Writer,
		Now:    time.Now,
		Sleep:  time.Sleep,
	}
}

// W returns d's Writer, or a refusing one if the field was never populated.
//
// Call sites use d.W() rather than d.Writer for one reason: a Deps built as a
// struct LITERAL — which every test in this tree does — has a nil interface
// there, and a nil interface method call is a panic rather than a permission
// fault. The whole point of the denied handle is that an ungranted capability
// fails as a refusal you can read; a nil deref would undo that at exactly the
// call sites least likely to be exercised.
func (d Deps) W() capability.Writer {
	if d.Writer == nil {
		return capability.Denied()
	}
	return d.Writer
}

// GrantedBy scopes d's Writer to what b declared.
//
// It takes a BINDING rather than an extension because grants are per-binding: an
// extension holding a read assertion and a write transition must not be able to
// mutate from inside the assertion. Callers pass the specific binding their lane
// runs under.
func (d Deps) GrantedBy(b extension.Binding) Deps {
	d.Writer = capability.For(b).Writer
	return d
}

// PollUntil calls cond immediately, then every interval until it returns true or
// timeout elapses (mirrors the bash `until … sleep N` loops). now/sleep are
// injected so tests run without real waiting.
//
// Boundary: the loop gives up when !now().Before(deadline), so a probe landing
// EXACTLY on the deadline is the last one tried. The three loops collapsed into
// this one disagreed here — waitPoll used now().After(deadline), which grants one
// extra sleep+probe round at the exact boundary. !Before is kept: it is the
// stricter reading of "within the budget", it was already what two of the three
// call sites did, and it makes a fake clock that lands exactly on the deadline
// terminate instead of spinning. With a real clock the two spellings differ only
// on an exact-nanosecond tie, which is why nothing observable changes here.
// Second bound: an ATTEMPT CAP, because the deadline alone is not enough. now and
// sleep are independent seams, and a caller that injects a REAL clock with a no-op
// sleep — the combination every bootstrapDeps fake in this package uses — makes the
// deadline unreachable in any useful time and turns this into a busy-spin for the
// full real timeout. That hung the test suite once already (see
// waitAplGitConfig/353ea2de). Under a real clock the cap and the deadline expire at
// essentially the same iteration, so nothing observable changes in production; the
// cap simply cannot be defeated by a clock that does not advance.
func PollUntil(now func() time.Time, sleep func(time.Duration), timeout, interval time.Duration, cond func() bool) bool {
	deadline := now().Add(timeout)
	attempts := 1
	if interval > 0 {
		attempts = int(timeout/interval) + 1
	}
	for attempt := 1; ; attempt++ {
		if cond() {
			return true
		}
		if !now().Before(deadline) || attempt >= attempts {
			return false
		}
		sleep(interval)
	}
}

// EnvOrDefault returns getenv(key), or def when it is unset/empty. getenv is
// injected so env parsing is unit-testable without mutating the process.
func EnvOrDefault(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

// EnvOr is EnvOrDefault against the real process environment.
func EnvOr(key, def string) string { return EnvOrDefault(os.Getenv, key, def) }

// WriteTempKubeconfig writes raw kubeconfig bytes to a tempfile named with the
// given CreateTemp pattern and returns its path plus a remove cleanup. A failed
// write cleans up its own partial file, so the caller only has to defer cleanup
// on the success path.
func WriteTempKubeconfig(pattern string, raw []byte) (string, func(), error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", nil, fmt.Errorf("create kubeconfig tempfile: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, fmt.Errorf("write kubeconfig: %w", err)
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// RunCombined runs cmd with stdout+stderr captured into one buffer and returns
// (combined output, exit-0). The run MUST happen before the buffer is read:
// `return buf.String(), cmd.Run() == nil` evaluates its operands left-to-right,
// snapshotting the buffer EMPTY before the command ever executes. That exact
// bug made every kubectl call return "" on the e2e bootstrap (the
// "empty kubeconfig" color.Red herring) — see TestRunCombined_OutputAfterRun.
func RunCombined(cmd *exec.Cmd) (string, bool) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	ok := cmd.Run() == nil
	return buf.String(), ok
}

// ── output formatting ────────────────────────────────────────────────────────
//
// Shared by tfplan, the argo assert lane and converge: three callers that all
// need to quote a tool's output into an annotation without pasting a whole log.

// firstLine truncates a multi-line/long condition message to a single readable line
// for the progress log (the full text still lands in the deadline diagnostics).
func FirstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return strings.TrimSpace(s)
}

// tailLines returns the last n lines of s without a trailing newline
// (`tail -N` semantics: a final newline terminates the last line rather than
// starting an empty one). n <= 0 yields "".
func TailLines(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	all := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if n < len(all) {
		all = all[len(all)-n:]
	}
	return strings.Join(all, "\n")
}

// SplitCSVList splits a comma-separated env value, trimming blanks and dropping
// empties. TWELVE non-test callers — every ci verb that takes a list through the
// environment spells this, and they all spelled it slightly differently before it
// was collapsed here.

// splitCSVList splits a comma-separated flag into trimmed, non-empty entries.
func SplitCSVList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// WaitPoll is PollUntil with the real clock, and it came from
// internal/extensions/converge -- one line wrapping the function directly above
// it, in a package two peers then had to import to reach. The seamed version
// exists so tests can drive the loop without sleeping; this is the shape every
// non-test caller wants, and it belongs next to what it wraps.
// WaitPoll is pollUntil (ci_shared.go) against the real clock: it calls cond
// until it returns true or timeout elapses, sleeping interval between tries with
// an immediate first try. Returns whether cond succeeded within the budget.
func WaitPoll(timeout, interval time.Duration, cond func() bool) bool {
	return PollUntil(time.Now, time.Sleep, timeout, interval, cond)
}
