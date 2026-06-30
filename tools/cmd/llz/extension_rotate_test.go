package main

import (
	"strings"
	"testing"
)

// fakeRotator is a built-in Go implementation of TokenRotator — proving the
// interface is satisfied by more than the declarative adapter (origin-erased).
type fakeRotator struct{ n, secret, val string }

func (f fakeRotator) Name() string { return f.n }
func (f fakeRotator) Rotate(globalOpts) (RotationResult, error) {
	return RotationResult{Secrets: map[string]string{f.secret: f.val}}, nil
}

func TestTokenRotatorSatisfiedByBothOrigins(t *testing.T) {
	var _ TokenRotator = declRotator{} // declarative
	var _ TokenRotator = fakeRotator{} // built-in Go
	r := fakeRotator{"x", "TOK", "v"}
	res, _ := r.Rotate(globalOpts{})
	if res.Secrets["TOK"] != "v" {
		t.Fatal("built-in rotator should produce its value")
	}
}

func stubMint(t *testing.T, value string, calls *int) {
	t.Helper()
	orig := execCapture
	execCapture = func([]string) (string, error) { *calls++; return value, nil }
	t.Cleanup(func() { execCapture = orig })
}

func TestRotateMintsAndReseeds(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "fermyon",
		"schemaVersion: 2\nname: fermyon\nshort: x\nkind: tool\n"+
			"secrets:\n  - {name: FERMYON_CLOUD_TOKEN, bao: 'secret/fermyon#token'}\n"+
			"rotate: {argv: [./mint], secret: FERMYON_CLOUD_TOKEN}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"fermyon"}})

	var mints int
	stubMint(t, "fresh-token", &mints)
	bao, _ := stubSeed(t)

	if err := runExtensionRotate(globalOpts{yes: true}, root, ""); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("mint should run once, ran %d", mints)
	}
	if strings.Join(*bao, ",") != "secret/fermyon,token,fresh-token" {
		t.Fatalf("rotated value should re-seed its target; got %v", *bao)
	}
}

func TestRotateGatedWithoutYes(t *testing.T) {
	root := t.TempDir()
	installExt(t, root, "f",
		"schemaVersion: 2\nname: f\nshort: x\nkind: tool\nsecrets:\n  - {name: T, bao: 'secret/f#t'}\nrotate: {argv: [./mint], secret: T}\n", nil)
	saveExtConfig(root, extConfig{Enabled: []string{"f"}})
	var mints int
	stubMint(t, "v", &mints)
	bao, gh := stubSeed(t)
	if err := runExtensionRotate(globalOpts{yes: false}, root, ""); err != nil {
		t.Fatal(err)
	}
	if mints != 0 || *bao != nil || *gh != nil {
		t.Fatal("no --yes must neither mint nor write")
	}
}

func TestRotateOnlyNamed(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a", "b"} {
		installExt(t, root, n,
			"schemaVersion: 2\nname: "+n+"\nshort: x\nkind: tool\nsecrets:\n  - {name: T, bao: 'secret/"+n+"#t'}\nrotate: {argv: [./mint], secret: T}\n", nil)
	}
	saveExtConfig(root, extConfig{Enabled: []string{"a", "b"}})
	var mints int
	stubMint(t, "v", &mints)
	stubSeed(t)
	if err := runExtensionRotate(globalOpts{yes: true}, root, "a"); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("only the named extension should rotate, mints=%d", mints)
	}
	if err := runExtensionRotate(globalOpts{yes: true}, root, "ghost"); err == nil {
		t.Fatal("an unknown name should error")
	}
}

func TestLintRotate(t *testing.T) {
	base := func(r *extRotate, secs []extSecret) extManifest {
		return extManifest{Name: "x", Short: "y", Kind: "tool", Stage: StageUniversal, Secrets: secs, Rotate: r}
	}
	targeted := []extSecret{{Name: "T", Bao: "secret/x#t"}}
	cases := []struct {
		name string
		m    extManifest
		want int
	}{
		{"valid", base(&extRotate{Argv: []string{"./mint"}, Secret: "T"}, targeted), 0},
		{"inline shell", base(&extRotate{Argv: []string{"bash", "-c", "x"}, Secret: "T"}, targeted), 1},
		{"undeclared secret", base(&extRotate{Argv: []string{"./mint"}, Secret: "NOPE"}, targeted), 1},
		{"secret has no target", base(&extRotate{Argv: []string{"./mint"}, Secret: "T"}, []extSecret{{Name: "T"}}), 1},
		{"missing argv+secret", base(&extRotate{}, targeted), 2},
	}
	for _, c := range cases {
		if got := lintManifest(c.m); len(got) != c.want {
			t.Errorf("%s: %d findings, want %d (%v)", c.name, len(got), c.want, got)
		}
	}
}
