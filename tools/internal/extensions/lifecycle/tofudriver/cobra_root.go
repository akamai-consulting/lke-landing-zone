package tofudriver

import "os"

// tfApplyLinodeToken came with the apply path — its only caller. firstNonEmpty
// is a local three-line copy: package main keeps the original for its own
// callers, and a shared package for three lines would cost more than the copy.

// tfApplyLinodeToken reads the Linode PAT available on the terraform apply path.
// Deliberately NOT linode.TokenFromEnv: the apply step is handed its credential as
// TF_VAR_linode_token (terraform's own variable plumbing), not LINODE_API_TOKEN,
// so the fallback name differs and folding the two readers together would
// silently change which variable wins in jobs that set more than one. Returns ""
// when neither is set; the caller reports it, since the apply-path message
// carries the terraform exit code.
func tfApplyLinodeToken() string {
	return firstNonEmpty(os.Getenv("LINODE_TOKEN"), os.Getenv("TF_VAR_linode_token"))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
