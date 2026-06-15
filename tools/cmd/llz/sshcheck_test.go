package main

import "testing"

func TestNormalizeKnownHosts(t *testing.T) {
	host := "ghe.example.com"
	// Same key material, different scan order + an unrelated host line + a comment.
	a := "# comment\n" +
		"ghe.example.com ssh-ed25519 AAAAED\n" +
		"github.com ssh-rsa ZZZZ\n" +
		"ghe.example.com ssh-rsa AAAARSA\n"
	b := "ghe.example.com ssh-rsa AAAARSA\n" +
		"ghe.example.com ssh-ed25519 AAAAED\n"
	if normalizeKnownHosts(a, host) != normalizeKnownHosts(b, host) {
		t.Errorf("same keys (diff order) should normalize equal:\n a=%q\n b=%q",
			normalizeKnownHosts(a, host), normalizeKnownHosts(b, host))
	}
	// A rotated key must differ.
	c := "ghe.example.com ssh-rsa ROTATED\nghe.example.com ssh-ed25519 AAAAED\n"
	if normalizeKnownHosts(a, host) == normalizeKnownHosts(c, host) {
		t.Error("rotated key should not normalize equal")
	}
	// Only the requested host's lines are kept.
	if got := normalizeKnownHosts("github.com ssh-rsa ZZZZ\n", host); got != "" {
		t.Errorf("unrelated host should yield empty, got %q", got)
	}
}

func TestNonCommentLines(t *testing.T) {
	in := "# header\n\nghe.example.com ssh-rsa AAAA\n   \n# tail\nx ssh-ed25519 BBBB\n"
	want := "ghe.example.com ssh-rsa AAAA\nx ssh-ed25519 BBBB"
	if got := nonCommentLines(in); got != want {
		t.Errorf("nonCommentLines = %q, want %q", got, want)
	}
}
