package objenc

// deps.go — what the object-storage encryption gate is handed.
//
// READ-ONLY BY CONSTRUCTION. Linode applies SSE-S3 with keys it manages, so there
// is no key for the platform to mint, hold or hand out — the gate only observes.

// Deps carries what this package cannot reach for itself.
type Deps struct {
	// KubectlOut reads the cluster — the Secrets that carry object-storage
	// credentials, and Harbor's routes.
	KubectlOut func(args ...string) (string, error)

	// SecretField decodes one base64 field out of a kubectl-fetched Secret.
	SecretField func(raw []byte, field string) (string, error)
}
