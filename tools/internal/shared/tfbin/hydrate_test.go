package tfbin

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/tfenc"
)

func withLocalEnv(t *testing.T, extra []string, err error) {
	t.Helper()
	prev := localEnv
	localEnv = func() ([]string, error) { return extra, err }
	t.Cleanup(func() { localEnv = prev })
}

// The chokepoint's whole value is that no call site has to ask. If Command stops
// attaching the environment, `llz ci tf-output` and `llz ci fetch-kubeconfig-state`
// silently go back to failing in a local checkout with OpenTofu's "Invalid
// expression" — with nothing in llz's own output to say why.
func TestCommandAttachesTheInstanceEnvironment(t *testing.T) {
	withLocalEnv(t, []string{"TF_ENCRYPTION=doc", "AWS_ACCESS_KEY_ID=ak"}, nil)
	c := Command("init")
	if !slices.Contains(c.Env, "TF_ENCRYPTION=doc") || !slices.Contains(c.Env, "AWS_ACCESS_KEY_ID=ak") {
		t.Errorf("Command did not attach the hydrated variables: %v", c.Env)
	}
	// The inherited environment must still be there — this ADDS, it does not
	// replace. A command that lost PATH would not find its own binary.
	if len(c.Env) <= 2 {
		t.Errorf("the inherited environment was dropped; got only %v", c.Env)
	}
}

// CommandContext is the same chokepoint with a deadline, and it was the sibling
// that got missed the LAST time something was added here (the OpenTofu binary
// rename — see tfbin.go). Pinning both is the cheap way not to repeat it.
func TestCommandContextAttachesTheInstanceEnvironment(t *testing.T) {
	withLocalEnv(t, []string{"TF_ENCRYPTION=doc"}, nil)
	c := CommandContext(t.Context(), "plan")
	if !slices.Contains(c.Env, "TF_ENCRYPTION=doc") {
		t.Errorf("CommandContext did not attach the hydrated variables: %v", c.Env)
	}
}

// With nothing to add, Env must stay nil — the plain "inherit" that every
// existing call site and test already assumes. Setting it to a copy of
// os.Environ() would work today and would quietly freeze the environment at
// construction time, which is a different thing from inheriting it.
func TestCommandLeavesEnvAloneWhenThereIsNothingToAdd(t *testing.T) {
	withLocalEnv(t, nil, nil)
	if c := Command("version"); c.Env != nil {
		t.Errorf("Env should stay nil when hydration contributes nothing, got %v", c.Env)
	}
}

// Malformed cached material is reported, not fatal: tfbin is on the path of
// commands that have nothing to do with encryption, and OpenTofu is the authority
// on whether the missing variable actually mattered.
func TestCommandStillRunsWhenHydrationFails(t *testing.T) {
	withLocalEnv(t, nil, errors.New("bad passphrase in cache"))
	if c := Command("fmt"); c.Env != nil {
		t.Errorf("a hydration error must not half-apply an environment, got %v", c.Env)
	}
}

// A caller that assigns cmd.Env AFTER Command() must win outright. statepassphrase's
// verify pass depends on this: it pins TF_ENCRYPTION to the NEW key alone, and
// merging a fallback into it would let a root still on the OLD key verify — the
// exact false pass that licenses deleting a passphrase still in use.
func TestCallerAssignedEnvWinsOutright(t *testing.T) {
	withLocalEnv(t, []string{"TF_ENCRYPTION=hydrated"}, nil)
	c := Command("state", "pull")
	c.Env = append(os.Environ(), "TF_ENCRYPTION=new-key-only")
	if slices.Contains(c.Env, "TF_ENCRYPTION=hydrated") {
		t.Error("the hydrated value survived a caller's own cmd.Env assignment")
	}
}

// The hydration must not disturb WHICH binary is resolved — that is tfbin's
// original job and the reason the package exists.
func TestHydrationDoesNotChangeTheResolvedBinary(t *testing.T) {
	withLocalEnv(t, []string{"TF_ENCRYPTION=doc"}, nil)
	t.Setenv(tfBinEnv, "my-tofu")
	c := Command("init")
	// The override need not exist on PATH — only the CHOICE is under test, and
	// exec.Command records it in Args[0] whether or not it resolves.
	if c.Args[0] != "my-tofu" {
		t.Errorf("resolved binary = %v, want the $TF override", c.Args)
	}
}

// CommandResolved exists so a caller that already resolved the environment does
// not pay for it — or report it — a second time. The property is "it does not
// resolve", so the test makes resolving an outright failure rather than counting
// filesystem reads and hoping.
func TestCommandResolvedDoesNotReResolve(t *testing.T) {
	prev := localEnv
	t.Cleanup(func() { localEnv = prev })
	localEnv = func() ([]string, error) {
		t.Error("CommandResolved re-resolved the instance environment — the caller had " +
			"already done it, so this is a second filesystem walk, a second cache read, and a " +
			"second chance for the two answers to disagree")
		return nil, nil
	}
	c := CommandResolved([]string{"TF_ENCRYPTION=doc"}, "plan")
	if !slices.Contains(c.Env, "TF_ENCRYPTION=doc") {
		t.Errorf("the caller's environment was not applied: %v", c.Env)
	}
}

// Same additive contract as Command: nil means a plain inherit, so Env stays nil.
func TestCommandResolvedWithNothingToAddInherits(t *testing.T) {
	prev := localEnv
	t.Cleanup(func() { localEnv = prev })
	localEnv = func() ([]string, error) { t.Error("CommandResolved resolved"); return nil, nil }
	if c := CommandResolved(nil, "version"); c.Env != nil {
		t.Errorf("Env should stay nil when there is nothing to add, got %v", c.Env)
	}
}

// ONE SENTENCE, TWO PRINTERS. tfbin reports hydration from the chokepoint;
// tofudriver reports it after resolving the environment itself. They had separate
// implementations with separate pluralisation and had ALREADY drifted — "resolved
// 2 variables" against "resolved 2 Terraform variables" — so which wording an
// operator saw depended on which code path resolved their environment.
//
// Both sides now render tfenc.ResolvedNote, and this asserts the one this package
// prints is exactly that, rather than a copy that agrees today.
func TestHydrationNoteIsTheSharedSentence(t *testing.T) {
	var got string
	prev := noteWriter
	noteWriter = writerFunc(func(b []byte) (int, error) { got += string(b); return len(b), nil })
	t.Cleanup(func() { noteWriter = prev })

	withLocalEnv(t, []string{"TF_ENCRYPTION=doc", "AWS_ACCESS_KEY_ID=ak"}, nil)
	resetNote()
	_ = Command("plan")

	want := tfenc.ResolvedNote(2)
	if !strings.Contains(got, want) {
		t.Errorf("the hydration note is not tfenc's sentence.\n got: %q\nwant it to contain: %q", got, want)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
