package linode

import "github.com/akamai-consulting/lke-landing-zone/tools/internal/shared/cli"

// ── THE IN-CLUSTER LINODE CREDENTIAL: WHERE IT IS, AND WHAT IT IS CALLED ──────
//
// Both came from internal/extensions/credrotate, which ROTATES the credential --
// a capability. Where the token is mounted and how the PAT is labelled are facts
// about the credential itself, and the reconciler and teardown were each importing
// the whole rotation package to learn one of them.
//
// THE LABEL CARRIES A SCAR AND IT MOVED WITH IT: revoke-old selects EVERY profile
// token with this exact label, keeps the newest and revokes the rest. PATs are
// profile-scoped, so the Linode account is the shared namespace -- two instances
// that both have a deployment named `lab` would revoke each other's live
// in-cluster credential on a schedule, silently, monthly. The instance
// discriminator in the prefix is what prevents that.

// LinodeTokenFile is where the Deployment mounts the optional linode-api-token
// Secret volume. Package var so tests can point it at a fixture.
var LinodeTokenFile = "/var/run/secrets/llz/linode-api-token/token"

// InClusterLinodeToken resolves the in-cluster Linode token: LINODE_TOKEN env,
// else the optional Secret volume, else "" (not yet synced — callers no-op or
// error per their contract).
func InClusterLinodeToken() string {
	return cli.InClusterToken("LINODE_TOKEN", LinodeTokenFile)
}

// InClusterPATLabel is the Linode-side token label — also the drain target
// (keep-newest, like every rotated credential). Per-region: each region's
// OpenBao gets its own token, so revoking one region never breaks another.
func InClusterPATLabel(prefix, region string) string {
	// The instance discriminator matters here for the same reason it does on the
	// obj keys: runCredentialsPATRevokeOld selects EVERY profile token with this
	// exact label, keeps the newest, and revokes the rest. Two instances in one
	// Linode account that both have a deployment named `lab` would each revoke the
	// other's live in-cluster credential on a schedule — silently, monthly. PATs
	// are profile-scoped, so the account is the shared namespace.
	if prefix == "" {
		return "llz-incluster-" + region
	}
	return "llz-incluster-" + prefix + "-" + region
}
