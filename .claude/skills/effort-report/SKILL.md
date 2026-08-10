---
name: effort-report
description: Report what it cost to build LLZ and what it saves an adopter - raw hours, AI token spend, person-hour equivalent, PRs, and lines changed, plus a measured standard-project baseline covering the quality, security, delivery and time-to-delivery gap a hand-rolled Linode deployment leaves open. Use when asked "how long did this take", "how much did this cost", "what's the ROI", "how many tokens", "how does this compare to building it ourselves", or when preparing a stakeholder, exec or customer-facing summary of the effort behind the repo.
---

# Reporting the effort behind LLZ

Two audiences want this and they want different halves. **Internal/exec:** what
did it cost to build. **Adopter/customer:** what does it save me. Both halves
below. Every number is regenerable — never quote from memory, and never quote
this file's snapshot without re-running first.

## Always run the script first

```bash
.claude/skills/effort-report/scripts/effort-stats.sh
```

It reads primary sources only: git history for delivery and SLOC, and
`~/.claude/projects/<slug>/*.jsonl` for token and wall-clock figures. The repo
grows; a hardcoded number in a deck is wrong within a week.

**The script prints a `commit coverage` percentage. It is load-bearing.** Local
transcripts begin when *this machine* began, not when the repo did. At the
baseline below, coverage was 68% — so measured token and wall-clock figures are
a **floor**, and extrapolating to the whole project means dividing by that
fraction. Work done on other machines, in cloud sessions, or by CI-side agents
is not in the files at all. Say "at least" when quoting them.

## Baseline snapshot (2026-08-06, commit `e0fbc5c1`)

| | |
|---|---:|
| Merged PRs / PR numbers issued | 324 / 414 |
| Commits (excl. merges) | 801 |
| Lines added / deleted (ever) | 260,016 / 50,948 |
| Logical SLOC (code, blank+comment stripped) | 129,103 |
| Markdown (123 files, non-blank) | 18,704 |
| Test:production ratio | 1.11 : 1 |
| Human active hours | ~265 |
| AI tokens, raw | 9.2 B measured / ~13.5 B extrapolated |
| — of which output tokens | 23.0 M (0.25%) |
| API responses | 24,555 |
| Calendar span | 51 days, 41 with commits |

## The four numbers, and how each gets misread

**PRs (324 merged of 414 issued).** The ~90 unmerged are not waste to hide —
they are the dead ends, and quoting both numbers is what makes the 324
credible.

**Lines (260k added / 51k deleted).** Quote *churn*, not the 209k surviving
tree. 51k deleted lines is rework that genuinely happened. Use **logical** SLOC
(129k) for any downstream estimate; physical LOC over-credits a Go+YAML tree by
about a third and inflates everything computed from it.

**Hours (~265).** Two independent methods agree: commit-clustering gives 264h,
transcript wall-clock gives 227h over 68% of commits. Both measure *elapsed
session time*, not undivided human attention — a long agent run with
intermittent check-ins is indistinguishable from focused work. Say "active
hours", never "hours of focused engineering".

**Tokens (9.2 B raw / 23.0 M output).** The one that will be misreported if you
let it, in two different ways.

*First, the measurement.* A single API response is written to the transcript as
**several JSONL lines — one per content block** — and every one of them repeats
the full `usage` object. Summing lines overstates everything by the average
content-block count; on this repo that was **1.97x**. `token-usage.py`
deduplicates by `message.id`, which is unique per response. If a figure here
ever looks implausibly large, this is the first thing to check.

*Second, the framing.* **98.8% of the raw total is cache reads** — conversation
prefix re-read on each turn, the cheap path by design. The honest headline is
**output: 23.0 M**, 0.25% of raw. Never present a bare "tokens used" total: it
tracks conversation length and context size far more than work performed, and a
9-figure number invites a plausibility challenge that the output figure simply
does not.

**Expect the "a subscription can't cover that" objection, and concede the half
of it that is right.** Raw totals do sound impossible. But plan limits meter
*requests inside rolling windows*, not raw token throughput, and cache reads are
deliberately the cheap path — 9.09 B of them across 24,555 responses is a ~370 k
average context, which is just what long agentic sessions on a large context
window look like. The defensible per-unit figures are ~940 output tokens per
response and ~100 k output tokens per active hour. Both are unremarkable; lead
with those.

## Person-hour equivalent: ~12–15 person-years

What a conventional team would have spent. Three methods, deliberately
disagreeing — present the spread, not a false point estimate.

| Method | Result | Treat as |
|---|---:|---|
| COCOMO II, 129 KSLOC, `2.94 × K^1.0997`, favourable multipliers | ~29 py | ceiling |
| Industry rate, 25–40 logical SLOC/person-day all-in | 15–24 py | mid |
| Bottom-up by component (see below) | 6–10 py | floor |

Bottom-up detail: `llz` CLI 30–45 PM · test suite 10–15 PM · Terraform + Helm +
platform-apl 9–18 PM · CI harness, 48 guards and 54 assert lanes 6–12 PM · docs
4–8 PM · LKE-E/apl-core integration discovery 12–24 PM.

**Quote ~12–15 person-years** (range 8–25): a team of **5 senior platform
engineers for ~2.5 years**, or 7–8 for 2 years once coordination overhead is
priced in. Not fewer than four — the surface spans Go tooling, Terraform,
Kubernetes policy, CI/CD and Linode platform internals, and few individuals
cover all five.

The methods diverge ~3x because COCOMO was calibrated on projects that wrote
more from scratch, while expert bottom-up estimates are reliably optimistic. If
someone demands one number, give 12–15 and say the spread is real.

### Do not quote a compression ratio without the caveats

265 hours against 12–15 person-years is a nominal ~75–90x. It is technically
true and rhetorically dangerous. Three discounts belong with it:

- LOC-based baselines **credit the codebase for its own verbosity**. A 1.11:1
  test ratio is far above typical human practice (0.3–0.6:1); a team delivering
  equivalent assurance writes perhaps half those tests.
- The human baseline includes **overhead the solo-plus-agent path never pays** —
  hiring, onboarding, ceremony, review latency, handoffs. Real cost in the
  counterfactual; not code-production work.
- The 265 hours are **senior direction hours**, not average-team hours. Not
  interchangeable units.

And one factor pushing the other way, which is the honest core of it:
`docs/lessons-learned.md` and the guard inventory are a catalogue of
**root-cause investigation against an opaque live system** — Cilium `ipBlock`
missing cluster identities, the `Lease` MicroTime precision wedge, disjoint OBJ
endpoint namespaces, sealed-secrets key mismatch. That work does not compress
the way code generation does. Code production compressed ~100x; discovery
compressed maybe 3–5x. Since discovery is roughly a fifth of the effort, the
blended figure is real but the arithmetic flatters it.

## The standard-project baseline

Comparisons against an imagined DIY team are weak. Use this profile instead: it
is an **audited real Linode/LKE-E deployment**, competently built by an
experienced team under delivery pressure, and it is what "we'll just hand-roll
it" actually converges to. Present it as an anonymized composite — see framing
rule 7; never name the engagement, product, customer or repo it came from.

**Lead with the strengths, or the rest reads as a sales pitch.** The baseline is
disciplined work: provisioning tooling that forces an explicit `--kubeconfig`
and strips ambient `KUBECONFIG` so no env var can point an action at the wrong
cluster; per-tenant secrets gitignored at any depth to contain leak blast
radius; container images pinned to dated tags, never `latest`; a dedicated
cluster and cloud account per tenant, so isolation is real; and non-obvious
decisions carrying comments that explain themselves. None of what follows is a
craft failure. It is the structural ceiling of the pattern.

**The pattern is copy-fork.** A new tenant is a literal recursive directory
copy of the golden manifests, then hand-edited. Everything downstream follows.

| Dimension | Standard project (measured) | LLZ |
|---|---|---|
| Per-tenant manifests | ~25, forked per tenant | 0 — versioned charts + modules |
| Copies of the same manifest set | 3, **already drifted at one tenant** | 1 |
| CI jobs | 4, all image build/tag | build + lint + policy + real-cluster e2e |
| Automated gates | **0** — no test, lint, scan or policy step | 48 guards, 54 assert lanes |
| NetworkPolicies | **0 in the entire repo** | default, with the Cilium traps encoded |
| Manifests with resource limits | under half | enforced by admission policy |
| Secret-in-git boundary | a **comment**, upheld by human care | gitleaks + plaintext-guard + at-rest-guard |
| Cloud API token | manual re-drop every 6 months | OpenBao + ESO, automated rotation, age asserted |
| Infrastructure as code | **none** — imperative CLI calls | Terraform: plan, state, drift detection |
| Deploy path | human on a laptop holding a prod token | CI-driven, GitOps convergence |
| Upgrade to N tenants | hand-edit the tag, N times, unverified | version bump + `llz upgrade` |

Two findings do most of the persuasive work, because neither is an opinion:

1. **Zero NetworkPolicies.** On LKE-E that is default-allow — any compromised
   pod reaches every datastore and admin API in the cluster, behind an
   internet-facing ingress tier.
2. **Three copies of the manifests had already diverged with a single tenant
   live.** Not a prediction about scale; a measurement taken before scale.

**Where the baseline is right and LLZ agrees:** dedicated cluster and account
per tenant is the correct isolation posture for a paying customer, and it is
LLZ's assumption too. LLZ is a landing zone for many independent clusters, not
a multi-tenant platform. There is no architectural disagreement — only a
difference in how the Nth cluster gets built and kept correct.

**Where the baseline is genuinely cheaper:** at one or two clusters, forking
beats a landing zone. The machinery does not pay for itself until roughly the
third cluster, or until a compliance requirement lands — whichever comes first.
Say this out loud. It is what makes the rest credible, and a technical audience
will otherwise say it for you.

## Adopter savings: 9–18 months and ~2.5–5 person-years

**Measure against what the adopter would otherwise build, not against LLZ's
12–15 person-years.** Nobody rebuilds all of LLZ; they build the thinner thing
profiled above and hit the same wedges. Conflating the two is the fastest way to
lose a technical audience.

| Phase | Standard project | With LLZ |
|---|---|---|
| First converging LKE-E + apl-core cluster (VPC, OBJ, bootstrap) | 4–8 weeks, 2–3 engineers | 2–5 days |
| Enterprise posture — OpenBao/ESO, credential rotation, Cilium NetworkPolicies, Kyverno, encryption at rest, image signing, Harbor, CIS evidence | 6–12 months, 3–4 engineers | ships with it |
| Keeping it working — e2e gates, upgrade path, drift guards | **not built at all** in the audited baseline | 54 assert lanes, 48 guards |

Net: **9–18 months of calendar time for a 3–4 person platform team**, or
**~2.5–5 person-years**. Phase 3 is the one to press on — the baseline confirms
small teams do not build it, and its absence is where the drift outages come
from a year later.

The most defensible single argument is the wedge catalogue, because each entry
is a failure that actually happened: **48 PR-time guards and 54 e2e assert
lanes**, each one a documented production wedge with a gate that fails when it
recurs, plus 21 runbooks and 35 ADRs/design docs. At even half a day of
debugging apiece — generous, given several cost days — that is ~50 engineer-days
of pure discovery, and only for the failures the team would have eventually
found. The ones found by an outage instead cost more.

What LLZ does **not** save: Linode account and entitlement lead time, org
identity decisions (domains, CIDRs, OIDC, registry), and the post-bootstrap
manual steps. Quote `docs/quickstart.md` for the real path and see
[onboard-adopter](../onboard-adopter/SKILL.md) — an adopter's realistic
first-cluster window is 1–3 weeks including account provisioning, not the 2–5
days of pure execution time.

## Framing rules

1. **Re-run the script.** Numbers older than a week are wrong.
2. **Never a bare token total.** Always split cache reads from output, and lead
   with output. If a token figure draws disbelief, re-derive it before defending
   it — the first such challenge was correct and found a 1.97x counting bug.
3. **"At least" on token and wall-clock figures** — coverage is partial.
4. **Ranges, not point estimates,** for anything modelled rather than measured;
   keep measured and estimated figures visually distinct.
5. **Both PR numbers,** merged and issued.
6. **Adopter savings ≠ build cost.** Different denominators.
7. **The baseline is anonymous.** It is a real audited engagement, so describe
   the *pattern* and never the source: no product, customer, repo, team, cloud
   account, region, cluster name, image path or application component. "A real
   audited LKE-E deployment" is the whole provenance anyone gets. This holds
   even when the audience is internal — decks leak forward into customer
   conversations, and the baseline's value does not depend on who it was.
8. **Lead with what the baseline does well.** A gap list that opens with the
   gaps reads as a sales pitch and gets discounted wholesale. The strengths are
   real and stating them first is what buys the findings their weight.
