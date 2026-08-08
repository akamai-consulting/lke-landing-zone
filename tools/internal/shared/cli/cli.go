// Package cli holds the small argument-parsing, env-default and JSON-record
// helpers shared by the credential-rotation commands (`llz credentials`,
// secret-rotation). They print one structured JSON record on stdout and read
// json.Number-typed values out of decoded Linode API responses.
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// EnvInt reads an int64 env var, falling back to def when unset/invalid.
func EnvInt(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// EnvBool reads a bool env var, falling back to def when unset/invalid.
func EnvBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// MustUint parses a uint64 flag value, exiting(2) on a malformed number.
func MustUint(s string) uint64 {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid unsigned integer %q\n", s)
		os.Exit(2)
	}
	return n
}

// PrintRecord marshals record to a single JSON line on stdout — the audit/SLA
// evidence the calling composite action parses.
func PrintRecord(record map[string]any) error {
	out, err := json.Marshal(record)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// AsUint64 extracts a uint64 from a json.Number (responses are decoded with
// UseNumber so numeric IDs survive as json.Number, not float64).
func AsUint64(v any) (uint64, bool) {
	if n, ok := v.(json.Number); ok {
		if i, err := strconv.ParseUint(n.String(), 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

// AsString returns v as a string, or "" if it is not a string.
func AsString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// InClusterToken resolves a credential from an env var, else a mounted file,
// else "". IT CAME DOWN FROM internal/extensions/credrotate, where it was written
// for the Linode token and where being a shared package's dependency made
// internal/shared/ghgitdata import an extension. Nothing in it is a rotation
// concern: it is the env-then-file lookup every in-cluster credential uses, which
// is why ghgitdata reached across a layer for it in the first place.
// InClusterToken is the shared secrets-before-apps token resolver: the named env
// var first (CronJob/CI compatibility), else the optional Secret volume mounted at
// file (kubelet-refreshed on rotate), else "" (not yet synced). Backs both the
// linode and apl-values-repo resolvers.
func InClusterToken(envVar, file string) string {
	if t := os.Getenv(envVar); t != "" {
		return t
	}
	if b, err := os.ReadFile(file); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// ── THREE HELPERS OUT OF internal/extensions/onboard ─────────────────────────
//
// Prompt reads one trimmed line with a label, ReadEnvFile parses a KEY=VALUE
// file, SortedKeys orders a map for deterministic output. None of them is an
// onboarding concern; onboarding was simply the first thing in this tree that
// needed to ask a human a question and read a dotenv, so they were written there
// and `upgrade` then imported the whole wizard to reach them.

func Prompt(in *bufio.Scanner, label string) string {
	fmt.Printf("  %s: ", label)
	if !in.Scan() {
		return ""
	}
	return strings.TrimSpace(in.Text())
}

// ReadEnvFile parses KEY=value lines, ignoring blanks and # comments. Missing
// file → empty map.
func ReadEnvFile(path string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.TrimSpace(k)] = v
		}
	}
	return m
}

func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
