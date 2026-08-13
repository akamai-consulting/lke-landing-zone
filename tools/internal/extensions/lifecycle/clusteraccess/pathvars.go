package clusteraccess

// pathvars.go expands the shell-style variables a caller may embed in a
// kubeconfig path.
//
// WHY THIS IS GO AND NOT SHELL. Two composite actions accept a caller-supplied
// path, and both docstrings promise that `$HOME` and `$RUNNER_TEMP` expand at run
// time. GitHub does not expand anything in a `with:` value, so every caller sends
// the LITERAL text `$HOME/.kube/config` and something downstream has to do it.
//
// That used to happen by accident: the path was interpolated straight into the
// run: script, so bash expanded it. Closing the injection closed the expansion
// with it — silently, because the write still succeeded, into a directory
// literally named `$HOME`, and the step still reported available=true while every
// consumer read the real $HOME/.kube/config.
//
// The obvious repair is `eval echo "$P"`, which is what lke-runner-acl reached
// for. It is the one repair that hands the injection back: eval re-enters the
// parser, so a path of `$HOME/x";id;#` RUNS. Doing the substitution here is both
// safe (the result is only ever text) and single-sourced — the two actions had
// begun to disagree about which variables expand, which is how a documented
// contract quietly becomes two.

import (
	"fmt"
	"strings"
)

// pathVars are the variables the action docstrings promise to expand. Deliberately
// a closed list rather than os.ExpandEnv: expanding EVERY variable would let a
// caller-supplied path read arbitrary runner environment — including the secrets
// GitHub exports into it — and interpolate them into a filename.
var pathVars = map[string]bool{
	"HOME":             true,
	"RUNNER_TEMP":      true,
	"GITHUB_WORKSPACE": true, // the old shell interpolation expanded it; narrowing to
	// two names would have turned a working custom kubeconfig-path into a hard
	// error on upgrade, which is a worse trade than the one extra name.
}

// resolvePath expands p and REFUSES to return a path that still carries a shell
// reference.
//
// Leaving the reference visible was the first cut's answer, and it was wrong for
// the same reason the original bug was: nothing downstream errors. Both callers
// MkdirAll the parent, write the file, and report available=true — so an unset
// HOME produced a directory literally named `$HOME` and a green step, which is
// precisely the shape this whole change exists to delete. A path that cannot be
// resolved is a failure, not a filename.
func resolvePath(p string, lookup func(string) string) (string, error) {
	got := expandPathVars(p, lookup)
	if strings.Contains(got, "$") || strings.HasPrefix(got, "~") {
		return "", fmt.Errorf("cannot resolve %q: it still contains a shell reference after expansion (%q) — "+
			"only $HOME, $RUNNER_TEMP and $GITHUB_WORKSPACE expand, and they must be set in the environment", p, got)
	}
	return got, nil
}

// expandPathVars replaces `$VAR`, `${VAR}` and a leading `~` with their values.
// lookup resolves a variable to its value; an unset variable leaves the
// reference in place, which resolvePath then rejects.
func expandPathVars(p string, lookup func(string) string) string {
	if p == "" {
		return p
	}
	if home := lookup("HOME"); home != "" {
		if p == "~" {
			return home
		}
		if strings.HasPrefix(p, "~/") {
			p = home + p[1:]
		}
	}
	// SCANNED, NOT ReplaceAll'd. A plain replacement has no name boundary, so
	// `$HOMEBREW_PREFIX` became `/home/runnerBREW_PREFIX` — a mangled path that
	// still contains no `$`, so it sailed through the fail-closed check and got
	// written to with available=true. The whole variable name has to be read
	// before deciding it is one of ours.
	var b strings.Builder
	for i := 0; i < len(p); {
		if p[i] != '$' {
			b.WriteByte(p[i])
			i++
			continue
		}
		name, next := readVarName(p, i)
		if name == "" || !pathVars[name] {
			// Not one of ours (or `$` alone): copy it through untouched so
			// resolvePath can reject it rather than silently mangling the path.
			b.WriteByte(p[i])
			i++
			continue
		}
		val := lookup(name)
		if val == "" {
			// Unset: leave the reference intact for resolvePath to refuse.
			b.WriteString(p[i:next])
			i = next
			continue
		}
		b.WriteString(val)
		i = next
	}
	return b.String()
}

// readVarName reads the variable reference starting at p[i] == '$', returning the
// name and the index just past the reference. Both `$VAR` and `${VAR}` resolve to
// the same name, and nothing needs to tell them apart — an earlier signature
// returned which form it was and every caller discarded it.
func readVarName(p string, i int) (name string, next int) {
	j := i + 1
	if j < len(p) && p[j] == '{' {
		k := strings.IndexByte(p[j:], '}')
		if k < 0 {
			return "", i + 1
		}
		return p[j+1 : j+k], j + k + 1
	}
	for j < len(p) && (p[j] == '_' ||
		(p[j] >= 'A' && p[j] <= 'Z') || (p[j] >= 'a' && p[j] <= 'z') ||
		(p[j] >= '0' && p[j] <= '9')) {
		j++
	}
	return p[i+1 : j], j
}
