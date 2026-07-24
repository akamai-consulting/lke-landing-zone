// Package apl is the cloud-agnostic APL layer of the CLI: the code that speaks
// App Platform's own domain model — teams, apps, values, secrets, platform
// health — against a running APL control plane, independent of who provisioned
// or operates the underlying cluster.
//
// This is one of the "two altitudes" established by ADR 0002
// (docs/adr/0002-llz-as-apl-cli.md). The boundary rule: code here MUST NOT
// import internal/provider (Linode day-0, fleet). Provisioning is a substrate
// the APL layer stands on, reached only through the provider.ClusterProvider
// interface — never by importing a concrete cloud.
//
// Phase 0 (this package's introduction) is a seam, not a migration: the package
// is intentionally near-empty. Population happens in later phases as APL-domain
// operations (values render/reconcile, team/user/app, the secret surface) move
// out of package main. See ADR 0002 Appendix A for the target package map.
package apl
