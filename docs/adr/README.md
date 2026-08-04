# Architecture Decision Records

Dated records of decisions that were expensive to make and would be expensive to
re-litigate. An ADR describes what was decided **at a moment in time** — including
things later rejected, renamed, or built differently. That is the point of the
format, so an ADR that names a flag which no longer exists is not stale; it is
doing its job. (`llz ci docs-guard` exempts this directory from its
command/flag check for exactly that reason, and still checks the links.)

## Index

| # | Decision | Date | Status |
|---|---|---|---|
| 0001 | *Reserved* — PAT rotation locus | — | **Not written.** Cited by 0007, 0009, 0012 as the "where does the credential live" blast-radius framing |
| [0002](0002-thin-terraform-native-bootstrap.md) | Thin Terraform: the in-cluster bootstrap runs natively, not as a TF workspace | 2026-07-15 | Accepted |
| [0003](0003-vendor-actions-and-bodies-into-instances.md) | Vendor reusable workflow bodies + composite actions into each instance | 2026-07-16 | Accepted |
| [0004](0004-decouple-openbao-write-identity-from-cluster-access.md) | Decouple the OpenBao write identity from cluster access | 2026-07-21 | Accepted |
| [0005](0005-managed-app-platform.md) | Pivot to Linode Managed App Platform | 2026-07-23 | Accepted |
| [0006](0006-managed-default-apps.md) | Managed default app set | 2026-07-23 | Accepted |
| [0007](0007-terraform-state-encryption.md) | Terraform state encryption at rest | 2026-07-28 | Accepted |
| [0007](0007-app-delivery-boundary.md) ⚠️ | App-delivery boundary: LLZ ships the platform, not your delivery chart | 2026-07-28 | Accepted |
| [0008](0008-opentofu-migration.md) | OpenTofu migration | 2026-07-28 | Accepted |
| [0009](0009-unmeasurable-credential-coverage.md) | Unmeasurable credential coverage | 2026-07-28 | Accepted |
| [0010](0010-in-cluster-mtls.md) | In-cluster mTLS | 2026-07-28 | Accepted |
| 0011 | *Reserved* — ambient mesh migration | — | **Not written.** Cited by 0010 and 0012 as where the remaining plaintext residuals close |
| [0012](0012-credential-observability-gaps.md) | Credential observability gaps | 2026-07-30 | Accepted |
| [0013](0013-llz-as-apl-cli.md) | Reframe LLZ as the APL CLI: one binary, two altitudes | 2026-07-24 | Proposed |

## ⚠️ Two ADRs share the number 0007

`0007-terraform-state-encryption.md` and `0007-app-delivery-boundary.md` were both
authored on 2026-07-28 and both took the next free number. So a bare **"ADR 0007"
is ambiguous** — of the citations across the docs, most mean state encryption and
the rest mean the delivery boundary.

**They were not renumbered, deliberately.** Both are referenced from places a
rename would break for adopters who have already rendered an instance:
`0007-terraform-state-encryption.md` is named in the `encryption.tf` comment that
ships into every Terraform root, and `0007-app-delivery-boundary.md` is linked from
`playbooks/first-workload.md`, which `deliver-docs` copies into every instance. The
cost of the rename lands on adopters; the cost of the collision lands on us.

**So: always qualify the citation** — "ADR 0007 (state encryption)" or "ADR 0007
(app-delivery boundary)" — and prefer linking the filename over writing the number.

A duplicate 0002 existed for the same reason and *was* resolved: the APL-CLI ADR
moved to 0013, since nothing outside this repo's own Go comments referenced it.

## Adding one

Take the next free number from the table above — **not** `ls | tail -1`, which is
how both collisions happened (two authors, same day, same command). If you claim a
reserved number (0001 or 0011), write the record and update this table in the same
change.
