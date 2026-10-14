package clusteraccess

// deps.go — what reaching a cluster is handed.
//
// One seam. `cluster-access` fetches a kubeconfig out of Terraform state and keeps
// the runner-IP allowlist in step with it, so what it cannot supply itself is the
// shell-out — and the terraform binary behind it now comes from internal/tfbin
// rather than a Deps field, because eight callers wanted it and one of them is
// this package.

// Deps carries what this package cannot reach for itself.
type Deps struct {
	// Exec captures a command's stdout. The Linode CLI and kubectl, mostly; the
	// terraform invocations build their own commands through internal/tfbin.
	Exec func(name string, args ...string) ([]byte, error)
}
