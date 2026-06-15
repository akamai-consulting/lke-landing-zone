package main

import (
	"encoding/base64"
	"fmt"
	"testing"
)

func TestFindSSHRepoSecret(t *testing.T) {
	urlB64 := base64.StdEncoding.EncodeToString([]byte("git@ghe.example.com:org/repo.git"))
	keyB64 := base64.StdEncoding.EncodeToString([]byte("-----BEGIN KEY-----"))
	js := fmt.Sprintf(`{"items":[
		{"metadata":{"name":"other"},"data":{"url":"%s"}},
		{"metadata":{"name":"ssh-repo"},"data":{"url":"%s","sshPrivateKey":"%s"}}
	]}`, base64.StdEncoding.EncodeToString([]byte("https://github.com/x")), urlB64, keyB64)

	name, hasKey, found := findSSHRepoSecret(js, "ghe.example.com")
	if !found || name != "ssh-repo" || !hasKey {
		t.Errorf("got name=%q hasKey=%v found=%v", name, hasKey, found)
	}

	if _, _, found := findSSHRepoSecret(js, "nope.example.com"); found {
		t.Error("should not match an absent host")
	}
	// secret without sshPrivateKey
	js2 := fmt.Sprintf(`{"items":[{"metadata":{"name":"r"},"data":{"url":"%s"}}]}`, urlB64)
	if _, hasKey, found := findSSHRepoSecret(js2, "ghe.example.com"); !found || hasKey {
		t.Errorf("expected found with hasKey=false, got found=%v hasKey=%v", found, hasKey)
	}
}

func TestKnownHostsHas(t *testing.T) {
	kh := "github.com ssh-rsa AAAA\nghe.example.com ssh-ed25519 BBBB\n"
	if !knownHostsHas(kh, "ghe.example.com") {
		t.Error("should find ghe.example.com")
	}
	if knownHostsHas(kh, "gitlab.com") {
		t.Error("should not find gitlab.com")
	}
	// substring must not match across the host boundary
	if knownHostsHas(kh, "ghe.example") {
		t.Error("prefix-without-space should not match")
	}
}

func TestSelectPlatformApps(t *testing.T) {
	js := `{"items":[
		{"metadata":{"name":"platform-openbao"},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}},
		{"metadata":{"name":"llz-cert-automation"},"status":{"sync":{"status":"OutOfSync"},"health":{"status":"Healthy"}}},
		{"metadata":{"name":"some-user-app"},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"}}}
	]}`
	apps := selectPlatformApps(js)
	if len(apps) != 2 {
		t.Fatalf("got %d apps, want 2: %+v", len(apps), apps)
	}
	names := map[string]bool{}
	for _, a := range apps {
		names[a.Name] = true
	}
	if !names["platform-openbao"] || !names["llz-cert-automation"] || names["some-user-app"] {
		t.Errorf("wrong selection: %+v", apps)
	}
}

func TestSSHAuthErrorRegex(t *testing.T) {
	if !sshAuthError.MatchString("fatal: Host key verification failed.") {
		t.Error("should match host key verification")
	}
	if !sshAuthError.MatchString("git@ghe: Permission denied (publickey).") {
		t.Error("should match permission denied")
	}
	if sshAuthError.MatchString("cloning ok, synced") {
		t.Error("should not match clean logs")
	}
}
