// Package provider is the substrate layer of the CLI: cloud / day-0 provisioning
// and fleet concerns (VPC/CIDR, firewall, account ACL, cluster lifecycle) that
// App Platform has no model for. Linode is the default — and, at Phase 0, only —
// implementation.
//
// This is the second of the "two altitudes" established by ADR 0013
// (docs/adr/0013-llz-as-apl-cli.md). The APL layer (internal/apl) reaches
// provisioning ONLY through ClusterProvider, never by importing a concrete
// cloud, so a future non-Linode provider drops in without touching APL-layer
// code. ADR 0013 §Phase 3 hardens this interface from a seam into a
// load-bearing boundary.
package provider

import "context"

// ClusterProvider abstracts the day-0 substrate: standing up (or attaching to) a
// Kubernetes cluster that App Platform can run on. It is deliberately minimal at
// Phase 0 — a seam that names the boundary rather than a finished contract. Real
// capabilities (VPC/CIDR, firewall, node pools, account ACL) accrete as the
// Linode day-0 code in package main moves behind it; see ADR 0013 Appendix A.
type ClusterProvider interface {
	// Name identifies the provider implementation, e.g. "linode".
	Name() string

	// Kubeconfig returns an admin kubeconfig for the named deployment — the one
	// capability every APL-layer operation needs from the substrate.
	Kubeconfig(ctx context.Context, deployment string) ([]byte, error)
}
