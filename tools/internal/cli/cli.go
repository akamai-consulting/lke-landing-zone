// Package cli holds the small argument-parsing, env-default and JSON-record
// helpers shared by the credential-rotation commands (`llz credentials`,
// secret-rotation). They print one structured JSON record on stdout and read
// json.Number-typed values out of decoded Linode API responses.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// Arg returns argv[i], or exits(2) if the index is past the end (a flag was
// given its name but no value).
func Arg(argv []string, i int) string {
	if i >= len(argv) {
		fmt.Fprintln(os.Stderr, "missing value for trailing flag")
		os.Exit(2)
	}
	return argv[i]
}

// RotatorArgs is the set of global arguments every credential-rotation command
// shares: the Linode token, the --apply / ROTATION_APPLY arm flag, the leading
// subcommand name, and the remaining args left for per-subcommand parsing.
type RotatorArgs struct {
	Token string
	Apply bool
	Sub   string
	Rest  []string
}

// ParseRotatorArgs parses the global flags the rotators share (--linode-token,
// --apply) plus the leading subcommand name, leaving everything else in Rest for
// the subcommand to parse. Token defaults to $LINODE_TOKEN and Apply to
// $ROTATION_APPLY. It deliberately does not build a Linode client — that keeps
// this package free of an internal/linode dependency; callers construct the
// client from Token themselves.
func ParseRotatorArgs(argv []string) RotatorArgs {
	a := RotatorArgs{
		Token: os.Getenv("LINODE_TOKEN"),
		Apply: EnvBool("ROTATION_APPLY", false),
	}
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--linode-token":
			i++
			a.Token = Arg(argv, i)
		case "--apply":
			a.Apply = true
		default:
			if a.Sub == "" && len(argv[i]) > 0 && argv[i][0] != '-' {
				a.Sub = argv[i]
			} else {
				a.Rest = append(a.Rest, argv[i])
			}
		}
	}
	return a
}

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

// MustInt parses an int64 flag value, exiting(2) on a malformed number.
func MustInt(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid integer %q\n", s)
		os.Exit(2)
	}
	return n
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
