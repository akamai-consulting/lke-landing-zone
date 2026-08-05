# Playbooks — routine day-2 operations

How to *use* the platform once it is converged: getting access to a component,
creating an account, running your own workload. Where a
[runbook](../runbooks/README.md) is for when something is wrong, a playbook is
for a normal Tuesday.

These ship into every instance (`llz ci deliver-docs` keeps `playbooks/`), so
they are read by operators without this repo checked out.

## Start here

**[operator-onboarding](operator-onboarding.md)** — the doc that ties the rest
together for someone receiving day-2 responsibility for the first time. Read it
before the others.

## By task

| What you want to do | Playbook |
|---|---|
| Run your own application on the converged platform | [first-workload](first-workload.md) |
| Inspect, sync or debug Applications; understand sync waves | [argocd-ops](argocd-ops.md) |
| Read or write secrets; create an OpenBao account | [openbao-accounts](openbao-accounts.md) |
| Push or pull images; create a Harbor robot account | [harbor-accounts](harbor-accounts.md) |
| Reach dashboards, or get a Grafana login | [grafana-access](grafana-access.md) |
| Query logs | [loki-access](loki-access.md) |

> Two traps worth knowing before you start, both of which have bitten operators
> following an older copy of these files:
>
> - **`llz openbao set` dry-runs and exits 0 without `--yes`.** You can rotate a
>   password in the product, see success, and leave OpenBao stale.
> - **Loki runs `auth_enabled: true`.** The tenant header is mandatory and differs
>   per writer (`admins` / `platform`); the wrong tenant returns an empty `200`,
>   not a `401`.

## Adding one

1. **Open with `**Applies to:**`** and `**Related:**` — the shape every file here
   already uses.
2. **Give the whole command, including the flag that makes it real.** A procedure
   that silently no-ops is worse than a missing one.
3. **Add a row above.** A playbook nobody can find by task is a playbook nobody
   reads.
