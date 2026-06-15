package main

// sshcheck.go implements `llz doctor --ssh-host`: verify port-22 reachability and
// (optionally) that a committed known_hosts still matches the host's live SSH keys.
// An SSH-based GitOps source path (e.g. a self-hosted Git host) hinges on both.
// Opt-in (not run unless --ssh-host is given) so it adds no noise for adopters who
// don't use that path. Host-key fetch shells out to ssh-keyscan (a system binary,
// like doctor's other checks) rather than re-implementing it.

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

// checkSSHHost reports port reachability + (if knownHostsFile != "") whether the
// committed known_hosts matches the live keys. Returns an error if the host is
// unreachable or the committed keys are stale.
func checkSSHHost(host, port, knownHostsFile string) error {
	if port == "" {
		port = "22"
	}
	fmt.Printf("\nSSH host %s:%s:\n", host, port)

	// 1. Port reachability.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 5*time.Second)
	if err != nil {
		report(fmt.Sprintf("reachable on %s (check egress / VPN / cluster NP)", port), false)
		return fmt.Errorf("cannot reach %s:%s within 5s", host, port)
	}
	_ = conn.Close()
	report(fmt.Sprintf("reachable on port %s", port), true)

	// 2. Fetch live host keys (ssh-keyscan is part of openssh-client).
	if _, err := execLookPath("ssh-keyscan"); err != nil {
		report("ssh-keyscan available (install openssh-client to check host keys)", false)
		return nil
	}
	out, _ := execOutput("ssh-keyscan", "-T", "10", "-t", "rsa,ecdsa,ed25519", host)
	live := nonCommentLines(string(out))
	if live == "" {
		report("ssh-keyscan returned host keys", false)
		return fmt.Errorf("ssh-keyscan returned no keys for %s", host)
	}
	report("ssh-keyscan returned host keys", true)

	// 3. Diff against the committed known_hosts, if supplied.
	if knownHostsFile == "" {
		return nil
	}
	committed, err := os.ReadFile(knownHostsFile)
	if err != nil {
		report("known_hosts file readable: "+knownHostsFile, false)
		return fmt.Errorf("read %s: %w", knownHostsFile, err)
	}
	if normalizeKnownHosts(live, host) == normalizeKnownHosts(string(committed), host) {
		report("committed known_hosts matches live keys", true)
		return nil
	}
	report("committed known_hosts matches live keys (STALE — host rotated keys, or wrong host)", false)
	fmt.Fprintf(os.Stderr, "  regenerate with: ssh-keyscan -t rsa,ecdsa,ed25519 %s > %s\n", host, knownHostsFile)
	return fmt.Errorf("committed known_hosts for %s is stale", host)
}

// nonCommentLines drops blank + '#' lines.
func nonCommentLines(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if t != "" && !strings.HasPrefix(t, "#") {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, "\n")
}

// normalizeKnownHosts keeps only host's "<algo> <key>" pairs, sorted — so
// scan-order variation doesn't trip the comparison.
func normalizeKnownHosts(content, host string) string {
	var pairs []string
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == host {
			pairs = append(pairs, f[1]+" "+f[2])
		}
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "\n")
}
