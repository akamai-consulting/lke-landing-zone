package kube

// secretprobe_test.go — the Secret-probe tests, moved with their subject.
//
// They lived in ci_bao_seed_test.go because the helpers did. Seventh
// stranded-test find, and the same pattern as the sixth: the file was named for
// the VERB that happened to define the helper, not for what the helper is.

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestK8sSecretField(t *testing.T) {
	withKubectl(t, func(a string) ([]byte, error) {
		switch {
		case strings.Contains(a, "get secret good"):
			return []byte("aHVudGVyMg=="), nil // "hunter2"
		case strings.Contains(a, "get secret badb64"):
			return []byte("!!not-base64!!"), nil
		// Dotted keys must be jsonpath-escaped or kubectl resolves
		// .data.tls.crt as a nested path.
		case strings.Contains(a, `jsonpath={.data.tls\.crt}`):
			return []byte("Y2VydA=="), nil // "cert"
		default:
			return nil, errors.New("NotFound")
		}
	})
	if got := SecretFieldOf("ns", "good", "k"); got != "hunter2" {
		t.Errorf("SecretFieldOf good = %q", got)
	}
	if got := SecretFieldOf("ns", "dotted", "tls.crt"); got != "cert" {
		t.Errorf("SecretFieldOf dotted key = %q", got)
	}
	if got := SecretFieldOf("ns", "absent", "k"); got != "" {
		t.Errorf("absent Secret must read as empty, got %q", got)
	}
	if got := SecretFieldOf("ns", "badb64", "k"); got != "" {
		t.Errorf("bad base64 must read as empty, got %q", got)
	}
}

func TestDescribeSecretForDiagNeverLeaksValues(t *testing.T) {
	src, err := os.ReadFile("secretprobe.go")
	if err != nil {
		t.Fatal(err)
	}
	fn := funcBody(string(src), "func DescribeSecret")
	if fn == "" {
		t.Fatal("DescribeSecret not found")
	}
	if strings.Contains(fn, "{$v}") {
		t.Error("jsonpath emits {$v} — that is the secret VALUE, and this string is logged")
	}
	if strings.Contains(fn, ".data.") {
		t.Error("selects .data.<key> — that reads a secret value, and this string is logged")
	}
	if !strings.Contains(fn, "{$k}") {
		t.Error("expected the key-name jsonpath {$k}; did the query change shape?")
	}
	// It must also never hand back raw base64 by dumping the whole .data map.
	if strings.Contains(fn, "jsonpath={.data}") {
		t.Error("dumps the entire .data map (base64 values) into the message")
	}
}

// funcBody returns the source text of a top-level func, from its declaration to
// the next one. Good enough to assert on a single function's contents.
func TestNonEmptyFields(t *testing.T) {
	if got := nonEmptyFields("  a   b \n c  "); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("got %v", got)
	}
	if got := nonEmptyFields("   \n  "); len(got) != 0 {
		t.Fatalf("whitespace-only should yield nothing, got %v", got)
	}
}

// withKubectl swaps this package's Exec seam for one test.
func withKubectl(t *testing.T, h func(args string) ([]byte, error)) {
	t.Helper()
	prev := Exec
	Exec = func(name string, args ...string) ([]byte, error) {
		if name != "kubectl" {
			return nil, errors.New("unexpected command " + name)
		}
		return h(strings.Join(args, " "))
	}
	t.Cleanup(func() { Exec = prev })
}

func funcBody(src, decl string) string {
	i := strings.Index(src, decl)
	if i < 0 {
		return ""
	}
	rest := src[i+len(decl):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// withCombined swaps the stderr-carrying seam.
func withCombined(t *testing.T, h func(args string) (string, bool)) {
	t.Helper()
	prev := Combined
	Combined = func(_ string, args ...string) (string, bool) { return h(strings.Join(args, " ")) }
	t.Cleanup(func() { Combined = prev })
}

// DescribeSecret is a DIAGNOSTIC, and until this test it measured 0% — it called
// exec.Command directly, so nothing could drive it. A diagnostic nobody can run in
// a test is one nobody has seen the output of, which is the failure mode it exists
// to prevent in the first place.
//
// Each branch names a DIFFERENT cause, and that is the whole value: on managed vs
// self-install the platform's Secrets differ by name, and that rename is what broke
// keycloak-configure once.
func TestDescribeSecretNamesTheCause(t *testing.T) {
	t.Run("namespace unreadable is not a Secret problem", func(t *testing.T) {
		withCombined(t, func(args string) (string, bool) { return "forbidden", false })
		got := DescribeSecret("llz-openbao", "bootstrap")
		if !strings.Contains(got, "not readable") || !strings.Contains(got, "not the Secret") {
			t.Errorf("want an RBAC/access explanation, got %q", got)
		}
	})

	t.Run("missing Secret lists the alternatives", func(t *testing.T) {
		withCombined(t, func(args string) (string, bool) {
			switch {
			case strings.HasPrefix(args, "get namespace"):
				return "ok", true
			case strings.Contains(args, "get secrets"):
				return "keycloak-admin\nplatform-admin\n", true
			default:
				return "NotFound", false
			}
		})
		got := DescribeSecret("keycloak", "missing")
		if !strings.Contains(got, "does not exist") {
			t.Errorf("want a does-not-exist verdict, got %q", got)
		}
		for _, alt := range []string{"keycloak-admin", "platform-admin"} {
			if !strings.Contains(got, alt) {
				t.Errorf("must name %q as an alternative — the managed/self-install rename is the "+
					"failure this exists to catch; got %q", alt, got)
			}
		}
	})

	t.Run("present but empty says so", func(t *testing.T) {
		withCombined(t, func(args string) (string, bool) {
			if strings.HasPrefix(args, "get namespace") {
				return "ok", true
			}
			return "   ", true
		})
		if got := DescribeSecret("ns", "empty"); !strings.Contains(got, "NO data keys") {
			t.Errorf("want an empty-Secret verdict, got %q", got)
		}
	})

	t.Run("present lists its keys", func(t *testing.T) {
		withCombined(t, func(args string) (string, bool) {
			if strings.HasPrefix(args, "get namespace") {
				return "ok", true
			}
			return "ca.crt tls.key", true
		})
		got := DescribeSecret("ns", "there")
		if !strings.Contains(got, "ca.crt") || !strings.Contains(got, "tls.key") {
			t.Errorf("want the key names, got %q", got)
		}
	})
}
