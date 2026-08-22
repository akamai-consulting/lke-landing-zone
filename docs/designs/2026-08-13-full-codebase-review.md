# Full-codebase review — 2026-08-13

A record of a static review sweep over the whole repository at `origin/main`
@ `1276c08f`. It is a snapshot of what one high-effort pass found, not a task
list that was worked; nothing in the tree was changed. Filed under `docs/designs/`
because it is a dated record rather than an instruction — it deliberately names
flags, paths and symbols that were wrong or absent at that commit, and rewriting
it to match a later CLI would falsify the record.

The one part that is kept current is the RECOMMENDATION — which of these to fix
next. See [Status at `d37d83cb`](#status-at-d37d83cb--2026-08-21), re-checked 81
commits later, for what still holds and what has since been fixed. Everything
outside that section is as written on 2026-08-13.

## Scope and method

| Parameter | Value |
| --- | --- |
| Base | `origin/main` @ `1276c08f`, reviewed in an isolated worktree |
| Files | 1,849 of 1,852 tracked (`.gitignore` x2 and `LICENSE` excluded — nothing reviewable) |
| Size | ~230k lines |
| Partition | 49 chunks sized to ~4-8k lines, verified to cover every tracked path exactly once |
| Effort | high — broad coverage, may include uncertain findings |
| Mode | report-only; the review branch was byte-identical to `origin/main` throughout |
| Rounds | 1 full sweep (49 chunks) + 1 convergence round (5 chunks, round-1 findings excluded) |

Findings marked **verified** were confirmed by execution — building the binary,
running throwaway probe tests, rendering charts, dumping the Terraform provider
schema, running the linters. The rest are unverified reads and some will be
wrong. Treat the ranking as triage order, not as a defect list to patch blind.

Totals: **369 findings** — 29 critical, 142 high, 153 medium, 45 low.

## The through-line

Roughly a third of all findings are the same shape: a check that answers "fine"
when it could not read the thing it was checking. A dropped `ok` flag, `Exists`
where `ExistsOK` was needed, a nil slice from a failed read treated as an
empty-and-therefore-clean corpus, a PromQL `sum()` over absent series returning
an empty vector instead of zero.

Individually each is small. Collectively they mean the convergence gates, the
assert lanes, the credential-rotation health checks and several security guards
can all pass on a cluster nobody successfully queried. Many of these files carry
header comments describing the vacuous-pass hazard they were written to
eliminate, and then re-introduce it a few lines down.

## Convergence

The sweep was asked to loop until there were no findings. In report-only mode it
does not reach zero, and the measurement is worth keeping.

After the 49-chunk sweep, the five highest-severity areas were re-reviewed with
every round-one finding listed and explicitly excluded, against unchanged code.
Round two returned 28 findings that were genuinely new.

| Area re-reviewed | Round 1 | Round 2 (new) | Yield |
| --- | ---: | ---: | ---: |
| credrotate + statepassphrase | 10 | 5 | 50% |
| capability + clusterspec | 13 | 4 | 31% |
| platform-apl | 6 | 7 | 117% |
| instance-template | 7 | 5 | 71% |
| converge | 7 | 7 | 100% |
| **Total** | **43** | **28** | **65%** |

A 65% second-pass yield on unchanged code means the limiting factor is reviewer
attention per pass, not the number of defects remaining. Extrapolated across all
49 chunks a full round two would return roughly 240 further findings and a round
three perhaps 150. The sequence decays slowly and never terminates. A loop only
reaches zero if each round changes the code.

## Fix first

Fifteen findings ranked by blast radius.

1. **`Makefile:1171`** (verified) — `make lint` exits 0 having run nothing at all,
   including the entire gate suite, when the only working-tree changes are
   untracked new files. Every other finding here sits behind a gate that can be
   skipped this way.
2. **`statepassphrase/rotate.go:254`, `cobra_statepassphrase.go:41`,
   `llz-secret-rotation.yml:496,514`** — a four-part chain over the most
   destructive credential in the instance. `--roots-dir` defaults to `terraform`
   but the roots live under `terraform-iac-bootstrap/`; the workflow's
   `aws-init-only` module resolves to a path that exists nowhere; and an
   all-roots-skipped rollover exits 0 printing that the old passphrase can now be
   deleted, which is the exit status the workflow gates deletion on. Following it
   destroys every state file. The rotation is unreachable today, which is the only
   thing preventing the loss — fix the false green before fixing reachability.
3. **`capability/repowrite.go:105`** (verified) — `resolveForWrite` resolves only
   the target's parent, so a symlink at the leaf is followed and a write escapes
   the fence. `Repo.Resolve` refuses the same path, so the reader is strictly
   stronger than the writer, and the file header claims the escape is closed.
4. **`capability/forge.go:169`** (verified) — `classifyAPIMethod` misses pflag's
   attached shorthand, so `gh api -XDELETE …` classifies as a read and a
   cloud-read handle permits it. The spaced and `=` forms are handled and tested.
5. **`shared/openbao/openbao.go:483,497`** — neither the prior-version read nor
   the restore write checks its HTTP status. A 403/429/500 returns nil, so the
   dual-write reports a rollback that did not happen; an error body also decodes
   cleanly into the response struct, and the restore then writes a null data
   payload over a live secret.
6. **`cli/ci.go:83`** (verified) — dependency install runs at command-tree
   construction time, before flags are parsed, so `DryRun` is permanently false.
   Built the binary and ran the nudge verb with a stub `kubectl` on PATH: it
   issued real annotate and patch writes against both Argo Applications.
7. **`clusterspec/overlay.go:179` and `apl/overlay/overlay.go:112`** (verified) —
   two independent paths that overwrite apl-core's own configuration on the
   machine branch, one disabling the values-repo backend a managed instance runs
   on, the other pushing an empty object over the object-storage CR.
8. **`platform-apl/components/openbao/openbao-cert-watcher.yaml:168`** — the
   cert-watcher has zero egress under the chart's default-deny, because every
   allow rule selects a label the watcher does not carry. Its poll fails forever,
   so at renewal nothing restarts OpenBao and the secret-store cascade this
   Deployment exists to prevent is exactly what happens.
9. **`terraform-modules/llz-cluster/main.tf:70`** — control-plane ACL is hardcoded
   enabled while every CIDR input defaults to empty, and the module's own minimal
   example sets none. The cluster comes up unreachable, and the lifecycle
   `ignore_changes` on the ACL means re-applying with CIDRs cannot fix it.
10. **`kubernetes-charts/llz-cert-automation/templates/rbac.yaml:23`** (verified) —
    the runner service account cannot create Workflows or write workflow task
    results, so the entire cert-renewal flow is non-functional. A sibling chart
    hit and documented the same executor failure.
11. **`tokeninv/inventory.go:177`** (verified) — a dedupe guard skips both loop
    iterations when the environment is empty, so every GitHub secret reports
    absent without the probe ever running. Because absent is not unknown the
    verdict still reads ok, and the fleet-wide unconfigured-credential alert
    fires for everything.
12. **`credrotate/cobra_credentials_flagsets.go:79,146`** — an identity resolver
    landed with tests but no call sites while the delivered workflow was changed
    to depend on it. The apply-armed monthly run overwrites the Terraform state
    access keys in every infra environment with a key that cannot read the state
    bucket, and PATs are minted unlabeled so the daily reaper matches nothing.
13. **`credrotate/broadpat.go:260` and `inclusterpat.go:197`** — the grace window
    is measured from the superseded token's creation time rather than from when
    it was superseded, so the previously-live token is always older than the
    window and is deleted seconds after its replacement is published.
14. **`instance-template/.github/workflows/llz-terraform.yml:166,745`** — the PR
    plan job declares no environment while the credentials it needs are
    environment-scoped, so every PR plan dies at init; and the OpenBao bootstrap
    job does not depend on the databases apply, so the admin seed silently no-ops
    and the provisioning password stays live in state, build green.
15. **`dockerfiles/Dockerfile:47`** (verified) — the version ldflag names a symbol
    that no longer exists after the CLI move, so the linker drops it silently,
    every image reports a dev version, and the image-freshness guard takes its
    warn-and-skip path on every instance pipeline.

## Status at `d37d83cb` — 2026-08-21

Everything above is the record as written at `1276c08f` and stays that way. This
section is the part that had to move: **what to fix next**, re-checked against
`origin/main` @ `d37d83cb` — 81 commits and 171 files (+27,307 / −862) later.

Method, so the confidence is legible. Each of the fifteen ranked findings was
re-read in the current tree. Every other finding above whose file appears in
`git diff --name-only 1276c08f origin/main` was re-read too. A finding whose file
is byte-identical to the reviewed tree is carried forward unchanged without
re-reading it: a fix cannot have happened in a file nobody touched.

### The ranked list, re-checked

Fourteen of fifteen still hold. Line numbers are the current ones where they moved.

| # | Finding | At `d37d83cb` |
| ---: | --- | --- |
| 1 | `Makefile:1207` — `make lint` runs nothing on an untracked-only tree | **open**, verbatim: `git diff --name-only HEAD` still excludes untracked files and the empty-`CHANGED` branch still exits 0 |
| 2 | state-passphrase rollover: `rotate.go:254`, `cobra_statepassphrase.go:41`, `llz-secret-rotation.yml:496,514` | **open**, all four parts. `--roots-dir` still defaults to `terraform` while the roots are under `terraform-iac-bootstrap/`; `aws-init-only` still resolves nowhere; an all-skipped run still prints that the old passphrase can be deleted. The R2 half is also unchanged — `secret-rotation.yml` still has no `state-passphrase` scope and no `state-passphrase-apply` input, so the rollover is still unreachable |
| 3 | `capability/repowrite.go:105` — leaf symlink escapes the write fence | **open**; `resolveForWrite` still anchors on `filepath.Dir(abs)` |
| 4 | `capability/forge.go:165` — `gh api -XDELETE` classifies as a read | **open**; the switch still handles `-X`, `--method=` and `-X=` and nothing else, so an attached shorthand falls through to `ForgeRead` |
| 5 | `openbao/openbao.go:483,497` — `Rollback` checks neither HTTP status | **open**; both calls still go through the raw `c.do`, which returns the response unexamined. `readKV` 15 lines away does check |
| 6 | `cli/ci.go:86` — `DryRun` frozen false at tree-construction time | **open**; `installConvergeDeps(cliopts.Global)` is still the first statement of `ciCmd()`, and the comment above it still claims the globals are parsed by then |
| 7 | `clusterspec/overlay.go:179`, `apl/overlay/overlay.go:112` — the machine branch overwrites apl-core's own config | **open**; both files byte-identical |
| 8 | `openbao-cert-watcher.yaml:168` — the watcher has zero egress | **open**; file byte-identical |
| 9 | `terraform-modules/llz-cluster/main.tf:71` — ACL hardcoded on, every CIDR input empty | **open**. The file DID change — a `check` block for the create-time VPC binding, and `ignore_changes` widened to `vpc_id, subnet_id` — but nothing touched `enabled = true` or the empty defaults, and the widened `ignore_changes` leaves the "re-applying with CIDRs cannot fix it" half exactly as it was |
| 10 | `llz-cert-automation/templates/rbac.yaml:23` — the runner cannot create Workflows | **open**; file byte-identical |
| 11 | `tokeninv/inventory.go:179` — the dedupe guard skips both iterations | **open**; still `if scope == "" && env == ""`, so an empty env skips the probe entirely and every GitHub secret reports absent |
| 12 | `credrotate/cobra_credentials_flagsets.go:79,146` — identity resolvers with no call sites | **open**; the whole `credrotate` package is byte-identical |
| 13 | `credrotate/broadpat.go:260`, `inclusterpat.go:197` — grace window measured from creation | **open**; same |
| 14 | `llz-terraform.yml:166,792` — PR plan without an environment; OpenBao bootstrap without the databases apply | **open**, both halves. The file changed by 57 lines elsewhere; `plan-cluster-pr` still declares no `environment:` while consuming `secrets.TF_STATE_*`, and `bootstrap-openbao` still reads `needs: [apply-cluster, apply-object-storage]` |
| 15 | `dockerfiles/Dockerfile:47` — the version ldflag names a dead symbol | **FIXED** in `f0a4ea87`. The flag now stamps `internal/cli.Version`, and the comment block above it records the scar. `fd866602` closed the follow-on: a `dev-` stamp used to pass every pin check |

### Fixed further down the list

Four findings from the appendix and round two were fixed by work that was not
aimed at this document.

- `promote/gen.go:230` (C14) — `d8be9720`. `PlanWorkflow` now reads the on-disk
  stages BEFORE the rank count and compares them to the declared deployments on
  every path, so the `len(stages) < 2` branch can no longer pass green over a
  stale `promote.yml`.
- `callerperms/guard.go:174` (C25) — `c3f209ad`. `holdsWrite` became a
  three-level `holds(scope, want)`, so a `read` escalation is caught too. Found
  the same way the finding predicted it would bite: on a real branch, against a
  guard printing OK.
- `budget/count.go:46` (C23) — `2398b86f`, though not the defect as filed. The
  fix exempts `defaults: run:` mappings rather than correcting the `- ` indent
  arithmetic, so the specific miscount the finding measured on a `- run: |` step
  is still there; what changed is that ~14 phantom lines left the tally.
- `onboard/wizard.go:361` (R2) — `23d6d727`. The clobber guard now runs
  regardless of whether the repo resolved, and refuses when it cannot tell.
  The commit also establishes the push was never reachable, which the finding
  did not know.

Also worth recording as a partial: `doctor/linode.go:143` (C37) is fixed —
`SpecK8sPins` (renamed from `SpecK8sVersions` in `63390fd2`) now returns nil for
an absent env instead of falling through to every other one.

### What has not moved

Nothing in the nine failure classes has had its systemic fix. Spot-checks at HEAD:

- **Unreadable state reported as clean** — `kubectlprobe/probe.go:165`,
  `healthsla`, `converge/health.go`, `seedspecial` all byte-identical. There is
  still no vet-style check on the non-`OK` probe variants, and the class remains
  roughly a third of everything above.
- **Guards that cannot fire** — `versionpins/pins.go` was substantially rewritten
  (`pippins.go` is new, the header re-scoped), and `maskComments` still blanks
  whole-line comments only, so a trailing `# … ci-tofu:1.9.8` is still scanned as
  a live pin. `chartguard/version.go:143`, `plaintext/guard.go`,
  `atrest/atrest.go:455`, `monitoringlabel/guard.go:65` unchanged.
- **Code written, tested, never wired in** — `clusterspec/aplversion.go:136`
  still has no production caller for `aplChartVersionError` /
  `AplChartVersionWarnings`; the file changed only to move the baseline to
  `v6.2.0`. Both credential-identity resolvers unchanged.
- **Capability-fence bypasses** — `cloud.go` and `repo.go` changed by two comment
  lines each (grant censuses, 44→47 of 64→67 extensions). `CloudFor`, `KubeFor`
  and `RepoWriterAt` still read `b.Grants` directly.
- **Registry namespace split** (C28) — `commands.go` gained four rows and still
  keys `Extension` on Go package names while `gates.go` keys it on real extension
  names, so the two tables still cannot be joined.
- `assertplatform/deps.go:53` — `Install(d Deps) { deps = d }` is still a
  wholesale replace. The file gained a comment naming `LoadSpec`'s default as
  "the dangerous half", which is the finding restated rather than closed.
- `clusteraccess/acl_configmap.go:275` — still `kubectl apply` with the dead
  `isAlreadyExists` guard beside it.
- `instanceresolve/region_resolve.go:75` — `AccountRegions` still returns
  `len(ids) > 0` on a path that files no skip report.

### Surface this sweep never covered

About a third of the ~27.3k lines added since `1276c08f` — 9,381 across 24
files, tests included — is in code that either did not exist when the chunks were
partitioned or was rewritten past the version its chunk read. No chunk above
owns any of it:

- the k8s-version lane — `assertplatform/k8sversion.go`, `verdict.go`,
  `cobra_k8sversion.go`, `instanceresolve/k8sversion_resolve.go` (1,047 lines on
  its own) and the `linode/lke_versions.go` catalog rework (+620 lines onto a
  file C29 did read, which is the second kind)
- `upstreamupdates` — the `upgrade-pr` verb — and the two new template-upgrade
  workflows delivered with it
- two new guards — `mutabletags` and `runinjection` — neither of which addresses
  any class above
- `promote/stages.go`, `clusteraccess/pathvars.go`, `versionpins/pippins.go`,
  `templatecommit/cobra_assert_release_image.go`

The `dispatchwatch` gap recorded under **Known gaps** is unchanged for a reason
worth updating: the package still is not on `main`. It was never merged, so it
remains unreviewed rather than newly reviewable.

### What this says about the recommendation

The ranked order above survives with one deletion. That is not a comment on the
81 commits — they closed real defects, four of them from this list, and the two
new guards are the right shape. It is the convergence measurement showing up
from the other side: a report-only sweep produces a list that decays only when
someone works it, and this one was worked incidentally, by people fixing what
they were already touching. Items 1 through 14 are still the order to take them
in.

## Recurring failure classes

Ranked by count. Each is worth one systemic fix rather than N individual patches.

1. **Unreadable state reported as clean state** (~110). The dominant class,
   described under the through-line above. A vet-style check banning the non-`OK`
   probe variants outside a small allowlist would retire most of it.
2. **Guards that cannot fire** (~45). Static gates that pass because their scanner
   never sees what it is meant to catch — a pin extractor that requires a sibling
   key and so finds one of three real pins, an HCL comment stripper that skips the
   rest of the file when a line comment contains a glob, a regex that matches only
   the first invocation per line, an insecure-TLS check that both common curl
   spellings bypass. All currently report green.
3. **Documentation describing a system that no longer exists** (~40). `docs-guard`
   passes clean because it checks that referenced things exist, not that the
   description is true. Rot concentrates in the delivered operator set — the pages
   shipped into every instance — including a playbook resting entirely on a Secret
   the managed-only render deleted and a destructive-sweep runbook documenting the
   wrong resource prefix.
4. **Code written, tested, and never wired in** (~25). A function with unit tests
   and no production caller, while something downstream was changed to depend on
   it. Includes both credential-identity resolvers, the chart-version guard rails,
   an injected spec loader, and a whole in-cluster writer absent from its
   kustomization while its header claims it replaced a CI step that still runs.
5. **Capability-fence bypasses** (~20). The binding rules are enforced in one
   constructor and skipped by the four siblings that read grants directly, so a
   read-only assertion can obtain a mutating client; roughly 30 non-test sites use
   the bypassing form. Elsewhere a mutating path shells out directly while only
   its read goes through the fence.
6. **`--dry-run` that isn't** (~15). Beyond the confirmed nudge case: the two most
   destructive teardown verbs keep a delete-capable transport in dry-run, a spec
   toggle writes its environment file before dry-running the render, and one
   import verb's dry-run always fails because it enters a directory it never
   created.
7. **Mechanical renames that ran through string literals** (~12). A `sed` across
   identifiers also rewrote quoted text and comments, which is why every mutation
   run dies in harness validation on rejected flags, a gitignore no longer matches
   its default directory, and help text describes the suite in terms of a Go
   symbol. Worth one repo-wide grep.
8. **PromQL over absent series** (~8). `sum()` and `count()` return an empty
   vector rather than zero when their series are missing, so the comparison never
   evaluates. Two OpenBao alerts go silent on a total outage and two collector
   alerts can never fire. A sibling ruleset already solves this class with
   `absent()`.
9. **Retry and timeout budgets that do not bound anything** (~7). A wait that
   accumulates its budget in interval units while each probe can block for
   minutes, a retry branch that never consults its deadline, and a poll loop that
   busy-spins under the package's own test fakes.

## Known gaps

- The `dispatchwatch` package did not exist at `1276c08f` and was not reviewed;
  it arrived on a later branch.
- Severity labels were assigned per chunk by independent reviewers, so they are
  internally consistent within an area and only roughly calibrated across areas.
- A small number of findings were surfaced independently from two directions and
  are deliberately not collapsed, since the duplication is evidence.
- One reviewer's coverage of `docs/workflows/` returned after its parent
  summarised; those findings are folded into the appendix below.

## Appendix — findings by area

Each heading is one review chunk. Bracketed labels are the reviewing agent's own
severity. Lines beginning CLEAN, CLEARED, VERIFIED or NOTE record what was checked
and found correct, and are kept so the next reader does not re-derive them.

### C01 lifecycle/openbao
- sealkey.go:116 — never-rotate guard uses `kubectlprobe.Exists`; an unanswerable probe reads as absent and regenerates+applies fresh unseal key over the live Secret. Bricks unseal. Should be `ExistsOK`, fail closed. [HIGH]
- cilogin.go:97 — `llz ci openbao-login` only outputs to `$GITHUB_ENV`; `ghaout.Append` is a silent no-op when unset. In its primary Argo/CronJob/reconciler use case the token is minted, discarded, exit 0. [HIGH]
- cli.go:61 — `RunGet` calls `ghsecret.Mask(val)` which writes `::add-mask::` to **stdout** right before the value; every documented `$(llz openbao get …)` capture gets two lines under GITHUB_ACTIONS. teamlogin.go:246 documents and avoids this. [MED]
- ci_openbao_init.go:204 — CI generate-root loop submits all three recovery keys unconditionally instead of breaking on `complete` (regenroot.go:73 does break). Threshold<3 → errors after root minted, before decoded → live untracked root token. [HIGH]
- regenroot.go:85 — `RunRegenRoot` uses raw `baoread.ExecPod` not `ExecFn`, bypassing the 24-try transient retry; blip mid-quorum aborts and misreports as key mismatch. Also unstubabble/untested. [MED]

### C02 lifecycle/brownfield
- brownfield.go:48 — RunScan returns early unless `--yes`; documented read-only `llz import scan` writes no report, exits 0 printing "(dry-run)". Gate belongs on --dry-run. [HIGH]
- plan.go:118 — every non-CNPG db bucketed as "cache", rendered "rebuild, do NOT migrate…ephemeral"; self-managed postgres:15 StatefulSet presented as throwaway data. Split on Engine not Kind. [HIGH]
- init.go:70 — component toggles gated on --yes while new/env add/render run for real; documented init yields instance with every discovered component off, silently. [MED]
- init.go:65 — `--dry-run` init always fails: d.New only echoes copier, so withinDir chdirs into a never-created dir. [MED]
- aplvalues.go:81 — obj-storage provider chosen by randomized map iteration + break; objectRegion/obj_cluster differs between runs. [MED]
- workloads.go:477 — postgres-exporter/redis-exporter images match engine substring, reported as databases to migrate. [LOW]
- workloads.go:412 — HasPrefix(secret,cluster) matches CNPG's own -ca/-server secrets; DB's own pods listed as its clients. [LOW]
- brownfield.go:653 — per-pool node_count tfvar compared against cluster-wide total; false drift warning for every multi-pool source cluster. [LOW]
- NOTE: RunScan/RunInit/RunPlan have zero test coverage.

### C03 lifecycle/converge
- cobra_nudge.go:27 — the DryRun field on Deps frozen at pre-parse zero value (installConvergeDeps runs during tree construction, cli/ci.go:83); `llz ci nudge-argo --dry-run` mutates the live cluster. Exact hazard cliopts exists to prevent. [HIGH]
- health.go:1023 — checkLeases lists retired namespaces "openbao"/"cert-automation"; live names are llz-openbao/llz-cert-automation. Both silently skipped, reports "all Leases renewed". Regression guard only covers healthNamespaces. [HIGH]
- health.go:204 — ConvergeRetryHard branch never checks deadline; alternating hard-fail/in-progress polls past --budget forever until GHA job timeout, no verdict. [MED]
- health.go:944 — OpenBao leader count hard-fails from seal states never read when pods merely not-Ready (only konnectivity guarded); rolling restart can trip twice-in-a-row abort. [MED]
- health.go:750 — unreadable `get configmap -A` → LokiConfigText "" → reported CatOK "Loki not deployed"; converge exits 0 while Loki still filesystem-backed. [MED]
- health.go:1187 — unanswerable cluster-foundation probe fails open, rewrites genuine missing-default-deny CatFail into CatOK. [MED]
- incluster.go:54 — report-only suppression ::notice:: unreachable (ConvergenceExit already returned 0); suppressed hard-fail leaves no trace in job log. [LOW]

### C04 lifecycle/reconciler + reconcilelanes
- reconcilelanes/es_store_recovery.go:94 — s.lastReady set "true" BEFORE forceSyncESKinds; failed/partial fan-out consumes the notReady→Ready transition permanently, missed ExternalSecrets fall to ESO ~16m backoff, no resync retries. Restart-amnesty branch gets ordering right; this one doesn't. Only all-succeed path tested. [HIGH]
- reconciler/reconcile_manager.go:163 — `_ = r.watch(ctx, fire)` discards watch error (no log/metric), reconnect flat 1s with unconditional fire(). Permanently-failing watch → ~1Hz full-relist loop forever while llz_reconcile_up stays 1. [MED-HIGH]
- reconciler/reconcile.go:305 — serving keypair reloaded per handshake but ClientCAs pool built once at startup; llz-client-ca renewal → every scrape fails bad certificate, liveness on separate plaintext port so nothing restarts. [MED]
- reconciler/reconcile_leader.go:210 — expired-lease takeover MergePatch has no resourceVersion precondition; ≥3 replicas → two standbys both setLeader(true) for a renewInterval. Double-mint window the elector exists to prevent. [MED-LOW]
- reconcilelanes/argo_nudge.go:56 — lane patches the same Applications collection it watches, no already-nudged record; each patch re-fires the watch. CronJob's 3-min poll was the old bound, nothing replaced it. [MED-LOW]
- reconciler/reconcile_linode_token_wait.go:87 — new goroutine+ticker per watch re-establishment, tied to ctx not the attempt; under failing watch accumulates ~1/s across three lanes. [LOW]
- reconciler/reconcile.go:257 — both listeners share one errc but receive site wraps all as "metrics server: %w"; --health-addr bind failure names wrong flag. errc buffered at 1 with two writers. [LOW]
- reconciler/reconcile_health.go:104 — sampleOpenBaoPods returns on 404 without publishing, upsert-only gauges frozen at last healthy values. Same hazard already fixed for sampleConvergence/sampleCertificates. [LOW]

### C05 lifecycle/credrotate
- cobra_credentials_flagsets.go:146 — resolveObjBucketCluster never called from production; `llz credentials obj-key create` runs with empty --bucket-cluster. Apply-armed monthly run overwrites TF_STATE_ACCESS_KEY/SECRET_KEY in every infra-<deployment> env with a key that cannot read the state bucket. [CRITICAL]
- cobra_credentials_flagsets.go:79 — same gap for resolveRotationLabel; all four credentials {pat,obj-key} {create,revoke-old} verbs take --label ""; PATs minted unlabeled, daily reaper matches nothing (permanent no-op, PATs accumulate toward 100-token cap). [HIGH]
- broadpat.go:253 — `created, _ := linode.ParseTS(...)` drops ok flag; unparseable/missing created → epoch 0 → revoked regardless of GRACE_DAYS. If the just-minted PAT fails to parse, the token already published to every GitHub env secret is deleted. pat.go:118 handles this with `if !ok { continue }`. [HIGH]
- broadpat.go:200 — published-partial audit record (published_envs, new_pat_id) built and returned but discarded by RunRotateBroadPAT; operator recovering from partial fan-out can't tell which envs were updated. [MED]
- table.go:241 — comment claims OpenBao login deferred until a credential is due, but ensureBao() runs unconditionally before the IsDue check; unreachable OpenBao hard-fails a run that should be a no-op. [MED]
- ROOT CAUSE (first two): rotation_identity.go landed with tests but no call sites, while the instance workflow + composite action were changed to depend on it.

### C06 lifecycle/teardown
- drain_obj_buckets.go:363 — s3DeleteObjects counts <Error> in a body truncated to 16KiB; a fully-failing 1000-key batch reads as ~920 deleted. `stalled` never trips, total inflated, drain grinds all 200 pages then fails with the wrong message. [HIGH]
- cobra_root2.go:198 — dry-run `reap-volumes --wait-detach 600` gets the read-only binding, every DetachVolume POST refused at transport; wait never converges, burns full 10 min emitting a warning per volume per poll. [MED]
- teardown.go:360 — RunForceDelete (360) and RunDeleteVPC (488) hand newTeardownClient a hardcoded `true`, keeping a DELETE-capable transport in dry-run for the two most destructive verbs. Every other site narrows on Yes && !DryRun; extension.go's cloudBinding doc states this fence as the invariant. [HIGH]
- destroy_unwedge.go:150 — Phase 1 scales Argo controllers to 0 then immediately runs Phase 2; kubectl scale returns on spec write, so the still-terminating app controller can re-add resources-finalizer to Applications Phase 2 just cleared. Comment claims an ordering the code doesn't enforce. [MED]
- reap.go:183 — ReapFirewalls reconstructs only the module's DEFAULT firewall label, never var.firewall_label; on an instance that sets it, `llz reap --cluster-label X --yes` reports deleted=0 while the firewall leaks. --fw-label alone is dead (whole step nested under ClusterLabel != ""). [MED]
- teardown.go:141 — two ::error:: remediation lines interpolate orAll(volRegion), emitting `llz reap --region (all) --yes` — a shell syntax error. [LOW]

### C07 lifecycle/objenc + assertobjstore
- objenc/proxy_resign.go:244 — 32MiB repair cap gates the DECLARED length only; decodeAWSChunked buffers the whole body before checking. Peak ~4x documented bound; a few concurrent repairs OOMKill the 512Mi DaemonSet. [HIGH]
- objenc/assert.go:522 — checkEndpointResolvesToProxy folds a failed `kubectl get` into the "rewrite is absent" branch, reporting an RBAC/API failure as live unencrypted traffic. [MED]
- objenc/proxy.go:135 — credsLoader starts with empty `cur`, so "keep last known-good on read error" is false on the first call; a transient stat failure silently disables the #397 repair with both counters at 0. [MED]
- objenc/proxy_resign.go:260 — access-key-mismatch skip increments nothing; "every write needed repair and none got it" indistinguishable from "nothing needed repair". [MED]
- objenc/proxy_resign.go:327 — Content-Encoding deleted wholesale, but aws-sdk-go-v2 APPENDS aws-chunked to a caller's existing encoding; a gzip object loses its encoding metadata. [MED]
- objenc/proxy.go:219 — parseSSECKeyFile trims trailing \r/\n before length-checking; a raw key ending in 0x0A/0x0D (~0.8%) CrashLoops every proxy pod. [MED]
- objenc/assert.go:650 — keys[0].LastModified is the newest of the FIRST bucket only, not of the sample it is labelled as. [LOW]
- assertobjstore/roundtrip.go:203 — config text concatenated in Go map order; first-match regex extraction of endpoint/bucket nondeterministic when >1 data key matches. [LOW]

### C08 lifecycle/bootstrapcluster
- bootstrap_cluster.go:876 — waitAplGitConfig never retries the absent-Secret case; readAplGitConfig's kubectl-failure path returns a plain error not errAplNotBYOGitReady, so guard at :855 returns on attempt 1. Fresh managed cluster: apl-secrets/apl-git-config doesn't exist yet → kubectl exits 1 → terminal bootstrap failure, defeating the 10-min wait. Tests only exercise "Secret exists, repoUrl empty". [HIGH]
- bootstrap_cluster.go:1079 — migration-Job poll treats .status.failed != 0 as terminal, but Job sets backoffLimit: 1; first-pod failure sets failed=1 while retry pod still pending, deferred del() deletes the Job, killing the retry. [HIGH]
- bootstrap_cluster.go:1136 — `if git ls-remote | grep -q` makes a FAILED ls-remote (auth/network/rate-limit) indistinguishable from "branch absent"; a healthy apl-<env> gets force-pushed over with apl-core's abandoned Gitea tree. Exact regression skipIfDstExists exists to prevent. [HIGH]
- bootstrap_cluster.go:1148 — `grep -q "refs/heads/$BRANCH"` unanchored; `main` matches `maintenance`, `apl-primary` matches `apl-primary-backup`. [MED]
- bootstrap_cluster.go:429 — ResolveKubeconfig silently ignores an explicit --kubeconfig that is missing/zero-byte, falling to ambient ~/.kube/config; the destructive StorageClass delete+recreate can land on a different cluster while reporting success. [HIGH]
- bootstrap_cluster_manifests.go:108 — ghcr-pull-secret created in kube-system (call-site comment says argocd) and no imagePullSecrets reference exists anywhere in the repo; private-fork images still ImagePullBackOff. [MED]
- bootstrap_cluster.go:371 — waitManagedArgoReady is the last raw deadline loop; under the package's own now:time.Now + no-op sleep fakes it busy-spins a real 15 minutes instead of failing. [MED]
- bootstrap_cluster_mutation_test.go:135 — TestBootstrapClusterFailsWhenABridgeApplyFails passes vacuously: no clusterID so bootstrap aborts at normalizeLKEClusterID before any apply; fail-injection never fires. [MED]
- bootstrap_cluster.go:764 — empty APL_VALUES_REPO_TOKEN makes configureManagedApl return nil, the one state the surrounding 25-line comment declares terminal; only the delivered workflow's require-secret preflight prevents a green-but-inert bootstrap. [MED]
- bootstrap_cluster.go:675 — normalizeLKEClusterID rejects `lke-123` although its doc claims ^lke-?[0-9]+$ parses (only the literal `lke` prefix is stripped). [LOW]

### C09 lifecycle/releasepublish + chartpublish
- chartpublish/check.go:276 — extractPublishPins requires a sibling `repoURL:`, so pins defaulting to global.chartsRegistry are dropped. VERIFIED: real repo scan returns exactly ONE pin (llz-openbao-platform); llz-cluster-foundation:0.1.14 and llz-cert-automation:0.1.12 in kubernetes-charts/llz-argo-bootstrap-apps/values.yaml silently skipped — the exact charts the file header cites as the reason the check exists. len(pins)==0 guard can't fire. Reports green. [HIGH]
- releasepublish/pin_images.go:353 — waitForManifest calls pinBuildFailed without excluding runs older than the build it just dispatched; a previously-failed build for the same sha aborts the wait on poll zero. Once a build for a sha fails, --build-if-missing can never self-heal. All trigger-path tests stub pinBuildFailed to ("",false). [HIGH]
- chartpublish/check.go:150 — no `checked == 0` guard; if every pin resolves to a non-ghcr.io host the verb prints "0 pinned first-party chart(s) are published" and exits 0. [MED]
- chartpublish/check.go:178 — a single transient GHCR 429/5xx during the --publish-if-missing wait returns immediately, killing the self-heal it just dispatched. [MED]
- releasepublish/publish_charts.go:157 — a --selected value matching no chart publishes nothing and exits 0. [MED]
- chartpublish/cobra_chartpublish.go:44 — no --interval/--timeout validation; --interval 0 gives a zero sleep plus 600 retries (sibling assert-instance-pr-gates rejects exactly this). [LOW]
- chartpublish/check.go:251 — filepath.Rel(absRoot, treeRelativePath) errors, error discarded, File left empty → `::error file=,line=N`. VERIFIED. Latent (prod uses --root .). [LOW]
- releasepublish/pr_gates.go:227 — checkSeverity ranks CANCELLED above SUCCESS; a stale cancelled row alongside a later SUCCESS on the same head fails the verb with "returned no verdict". [PLAUSIBLE/MED]

### C10 lifecycle/identityconfig + clusteraccess
- clusteraccess/acl_configmap.go:272 — NotFound fallback uses `kubectl apply`, an upsert that computes deletions from the LIVE object's last-applied-configuration. Two runners racing to create kube-system/firewall-runner-acl → loser's apply deletes winner's lease key → winner evicted from the control-plane ACL on next reconcile. The isAlreadyExists(cout) guard beside it is dead code: apply never returns AlreadyExists. [HIGH]
- identityconfig/keycloak_gateway_alias.go:79 — hostAliases is patchStrategy:"merge" patchMergeKey:"ip" in the PodSpec, so the strategic patch APPENDS a second alias for the same hostname when the gateway ClusterIP changes. /etc/hosts resolves the stale dead IP first, JWKS fetch breaks, and the "gateway Service was recreated" branch never converges. [HIGH]
- identityconfig/openbao_configure.go:379 — `return steps` on a forge-resolution failure also skips keycloakTeamSteps. LLZ_FORGE=ghes without LLZ_FORGE_HOST → the whole keycloak/ auth mount and every <name>-writer policy/role silently omitted from a one-shot bootstrap step, permanently breaking `llz openbao login --team`, under a warning that only mentions GitHub-OIDC. [HIGH]
- clusteraccess/acl.go:319 — revoke returns hard errors on a corrupt state file / non-numeric cluster_id, contradicting its documented `if: always()` tolerance; also skips the ConfigMap release and never removes the bad file, so it keeps failing. [MED]
- clusteraccess/acl.go:148 — `--dry-run revoke` resolves the cluster and re-detects the egress IP, but real revoke acts only on the state file. Dry-run can hard-fail where real path no-ops, and can report removing a different IP than the recorded one. [MED]
- clusteraccess/acl_configmap.go:257 — expiredRunnerACLKeys adds an unaccounted 10s-timeout kubectl GET per retry, so the 90s phase budget trips after 2-3 attempts not the 8 budgeted; the eviction-deadlock reassert repair gets 2 shots instead of 7. [MED]
- clusteraccess/acl_configmap.go:251 — budget bail calls leaseOutcome(ip, attempt-1, …) with zero-value lastErr/lastOut, printing "the apiserver ANSWERED, so access works" (the opposite diagnosis) and possibly "0/8 attempts". [MED]
- identityconfig/cobra_identity_cmds.go:110 — shipped --admin help is self-contradictory and factually wrong: identity.PlatformAdminRole IS "platform-admin", the exact role every keycloak team role binds, so --admin DOES confer `llz openbao login --team`. [LOW]

### C11 lifecycle/database + statepassphrase
- HEADLINE CHAIN: `llz ci rotate-state-passphrase` is a false green over the most destructive credential in the instance — wrong module name, wrong --roots-dir, and an all-skipped run reports success and licenses deleting the old passphrase.
- statepassphrase/rotate.go:254 — all-roots-skipped rollover exits 0 and prints "TF_STATE_ENCRYPTION_PASSPHRASE_OLD can now be deleted", the exit status the workflow gates deletion on. [CRITICAL]
- statepassphrase/cobra_statepassphrase.go:41 — --roots-dir default (and the workflow's arg) is `terraform`, but the roots live at `terraform-iac-bootstrap/`; rotate_test.go bakes the same wrong prefix. [CRITICAL]
- instance-template/.github/workflows/llz-secret-rotation.yml:496 — module `aws-init-only` resolves to terraform-iac-bootstrap/aws-init-only, which exists nowhere; the job dies before llz runs. [HIGH]
- database/rotate_dbadmin.go:258 — waitDBActive never waits for the cluster to LEAVE `active`, so a still-propagating reset yields the old password and a catastrophic-sounding "RESET BUT NOT PERSISTED" false alarm. [HIGH]
- statepassphrase/rotate.go:73 — `sh -c "tofu state pull | tofu state push -"` without pipefail: a failed pull reads as a successful re-key. [HIGH]
- database/assert_database.go:144 — ReadCreds drops the OpenBao error, turning a transient read failure into a confident "half-seeded" verdict. [MED]
- database/pg_probe.go:203 — every ErrorResponse maps to pgRejected, so a pg_hba/connection-limit error reads as a rejected credential and sends an operator to rotate a good password. [MED]
- verbs/onboard/wizard.go:361 — PushSecrets skips DropStatePassphraseIfLive when repo resolution fails; the clobber guard fails open on the manual gather+push path. [MED]
- database/cobra_database.go:41 — help text still documents the state-refresh step that was deliberately removed. [LOW]
- database/db_report.go:34 — any os.ReadFile error (not just not-exist) reports declared=false, silently skipping the admin seed with nothing downstream to flag it. [MED]

### C12 lifecycle/render + deliverdocs + environments
- deliverdocs/deliver.go:78 — prune loop RemoveAll's every top-level docs/ entry not in platform.DeliveredDocs, and runs on every `copier update --trust` (copier.yml:134). Adopter-created files and subdirs under docs/ are deleted on every `llz upgrade`, directly contradicting docs/local.md's guarantee. [HIGH]
- render/render.go:526 — `if len(teams) > 0` means removing the LAST team from spec.teams emits no teams.yaml target, so it's neither rewritten nor drift-checked. `llz render --check` stays green while the stale committed teams.yaml keeps provisioning the removed team. Same shape at :513 for the obj overlay. [HIGH]
- deliverdocs/deliver.go:413 — rewriteDocLinks has no "exists in the template" gate (sibling RewriteInstanceRootLinks:260 refuses exactly this). It also walks adopter-owned docs/local.md, so an adopter's link to their own doc is silently rewritten to a template URL that 404s. [MED]
- deliverdocs/deliver.go:406 — rewriteDocLinks doesn't handle root-relative links: a root-relative link written from a runbook one directory down resolves against the runbook's own parent instead of the docs root, and is rewritten to a URL that 404s. Fourth copy of that resolution rule; the other three all strip the leading `/`. [MED]
- render/render.go:357 — ResolveLLZImageTag reads only llz_version and falls to "latest", while resolveTemplateRef falls back to _commit. An instance pinned by _commit alone fetches manifests at that SHA but runs :latest, with no ::warning:: (llzImageTagFor owns the warning and is never reached). [MED]
- environments/set.go:102 — `llz network add` writes args[0] into landingzone.yaml unvalidated; lz.Validate() runs validate.EnvName on network keys, so e.g. `Shared-Prod` is persisted then fails every subsequent render/env add/spec set with no recovery guidance. `llz env add` validates up front (add.go:54) for exactly this reason. [MED]

### C13 lifecycle/tofudriver + healthsla + harbor
- healthsla/readiness.go:186 — baoStatus returns ok=false on any non-zero exec exit, but `bao status` exits non-zero PRECISELY WHEN THE POD IS SEALED (and still prints valid JSON). Every sealed pod reported as "seal state UNKNOWN"; the `sealed` counter can never increment. baoread.ParsePodStatus's doc states the rule being broken. Test's sealed case returns (json, nil), a shape the real exec never produces. [HIGH]
- healthsla/readiness.go:128 — RunCertManager uses kubectlprobe.Items not ItemsOK; a failed apiserver read yields zero items and prints "All cert-manager Certificates Ready". Same false all-clear the ExternalSecrets branch 20 lines below guards against. [HIGH]
- tofudriver/cobra_tf.go:139 — shared-VPC deployments look the VPC up by `<cluster_label>-vpc` instead of vars.VPCNetwork (the label the cluster root's data.linode_vpcs.shared filters on). VPC id never resolves → subnet import always skipped → orphaned <cluster>-nodes subnet can never be re-adopted. With a stale dedicated VPC present, the subnet of the WRONG VPC gets imported. [HIGH]
- harbor/harbor_provisioner.go:232 — "delete the robot in Harbor UI" advice emitted per-robot but gated on robotsSeeded's global flag. Push-seeded/pull-unseeded makes it claim a fully-seeded path is "unseeded" and ask an operator to destroy a live push credential. [MED]
- harbor/cobra_harbor_lanes.go:31 — standby registry_host derived from HARBOR_URL with only a scheme TrimPrefix: no harborauth.UsableRegistryHost guard, no trailing-slash trim. Unset HARBOR_URL seeds registry_host:"" into OpenBao and reports success; every standby pull then fails looking like a credential error. [MED]
- healthsla/sla.go:155 — lokiObjkeyUpdatedTime collapses "token unset"/"exec failed"/"field absent" into ""; caller treats that as non-fatal warn returning nil. Drop OPENBAO_ROOT_TOKEN from the job and the hard 120-day SLA gate goes green forever. [HIGH]
- healthsla/sla.go:46 — same class: kubectlprobe.Items for the kube-system Secret list, so a read failing AFTER Reachable() passed reads as "no lke-admin-token exists" → warn + nil → the 90-day critical SLA cannot fire. [HIGH]
- healthsla/cobra_health_sla.go:23 — Deps.BaoExec(pod, addr, token, …) forwarded positionally into baoread.ExecFn(pod, token, stdin, …); addr→token slot, token→stdin slot. Inert today (both ""), but any future authenticated caller would pipe the token to the child's stdin and run unauthenticated. [MED]
- healthsla/cobra_health_sla.go:77 — help text still says "an unreachable pod counts as sealed", contradicting the unknown-vs-sealed split the code documents as a deliberate fix. [LOW]

### C14 lifecycle/promote+atrest+firewall+kyverno+gameday+branchpolicy
- branchpolicy/policy.go:95 — reviewer/wait-timer preservation reads envCfg["reviewers"]/["wait_timer"]/["prevent_self_review"], but GitHub returns those under protection_rules[]. Branch is dead, so the PUT always drops a paid repo's manually-configured required reviewers — the exact case lines 83-88 claim to prevent. [HIGH]
- gameday/wedge.go:85 — carvedAppNames() uses the UNFILTERED clusterspec.Components global while the renderer emits carved Apps only for ENABLED components. Any instance without objProxy/harbor/loki scores a containment breach on iteration 1; game-day fails with a false "NOT CONTAINED". Start gate (:112) and evalWedge (:70) also disagree about what an absent App means. [HIGH]
- branchpolicy/policy.go:61 — environment GET's error never classified; any transient failure (rate limit/403/500) falls into the create branch and fires a bare PUT at an existing environment, then rebuilds the "preserve" body from the post-PUT read. Under this file's own model that permanently wipes prod's protection rules on a network blip. [HIGH]
- atrest/atrest.go:455 — PROBE-VERIFIED. stripHCLNoise looks for `/*` before cutting `#`/`//`, so a line comment containing a glob like `modules/*` opens block-comment mode for the rest of the file. Brace counting stops, i=j-1 jumps to EOF, every later resource silently unscanned. Same fixture: 1 finding without the comment, 0 with it. False negative on a SECURITY gate. [HIGH]
- branchpolicy/policy.go:136 — all three mutating paths (proc.Run create :63, ghAPIBody PUT :137, POST :116) shell out to `gh` directly; only the read goes through capability.For(...).Forge. Comment at :234-240 asserts the opposite. Removing CloudMutate from the binding changes nothing. [MED]
- gameday/wedge.go:242 — setSelfHeal discards the read's ok; a transient jsonpath read failure plus a successful patch returns false, restore defer never registered, target Application left with selfHeal:false permanently — in a tool contracted to always restore. [MED]
- promote/gen.go:230 — PlanWorkflow returns Changed:false on the len(stages)<2 path without comparing to disk, so `llz env pipeline --check` passes green over a stale promote.yml that still chains (and applies to) deployments whose promotion_rank was removed. [MED]
- branchpolicy/policy.go:116 — step 3 enables custom_branch_policies:true, step 4 adds the rule separately. A non-plan-limit failure in step 4 aborts with the environment in custom-policy mode and zero rules, blocking ALL deploys to infra-<env>; no rollback, no self-heal on re-run, no message saying the env is locked out. [HIGH]

### C15 assertions/assertobs
- lokiwrites.go:79 — write-proof "cheap evidence" arm compares the bucket's newest object against the OLDEST ingester's start time with no freshness bound. Cluster up 30 days, writes broke 5 days ago → a 5-day-old chunk is still After(start) → reports PROVEN and skips the flush probe. The exact vacuous pass the file exists to eliminate. [HIGH]
- lokiwrites.go:207 — lokiNewestObject assumes objstore.SampleObjectKeys returns the bucket's newest, but that helper caps its LIST at 10 pages × 1000 keys in lexicographic order and sorts only what it listed. Loki chunk keys are fingerprint-hex-prefixed, so on any bucket >10k objects the newest chunk is outside the sample: the lane force-flushes every ingester on every run and can only ever report INCONCLUSIVE. [HIGH]
- promrulescheck.go:90 — isPrometheusRule/extractBareGroups use yaml.Unmarshal, which in yaml.v3 silently decodes only the FIRST document of a multi-doc stream (verified empirically). A PrometheusRule that isn't first is never promtool-validated; a multi-doc file is reported ok: after validating one document. The walked tree already contains multi-doc files (rbac.yaml has 12 `---`). [HIGH]
- readiness.go:51 — settle poll re-runs lokiProveWrites (up to 90s + a /flush POST to every ingester) on every attempt, so `--settle 120 --interval 10` gives ~2 readiness re-evaluations instead of ~12 and repeats the write side effect. [MED]
- readiness.go:363 — --name-match goes through regexp.MustCompile (also :65 and health.LokiConfigText); a malformed regex panics with a stack trace instead of the clean regexp.Compile error the sibling verbs return. [MED]
- cobra_readiness.go:62 — wait-harbor's help says "Exit 0 rolled out, 1 on timeout", but runCIWaitHarbor always returns nil (warning only). Help text contradicts deliberate documented behavior. [LOW]

### C16 assertions/assertsecrets + tokeninv
- tokeninv/inventory.go:177 — dedupe guard `if scope=="" && env==""` skips BOTH loop iterations when env is empty, so with REGION unset gatherSecretAges never calls the probe and reports all 10 GitHub secrets as `absent`. VERIFIED by throwaway test: 0 probe calls, 10 entries State:absent Expect:present. Because absent≠unknown, SecretProbeVerdict still says ok, so the reconciler publishes llz_credential_configured=0 for everything → LLZCredentialUnconfigured fires FLEET-WIDE and assert-rotation-health's presence lane fails every credential. Existing tests only ever pass env="infra-primary". Fix = index-based guard. [CRITICAL]
- assertsecrets/rotationhealth.go:373 — when llz_credential_secret_probe_ok=0 the writer publishes an empty llz_credential_configured family, so the !ok branch fires for every target with a message asserting "although the probe reported OK" and blaming a per-credential 403 that did not happen; ~9 false diagnoses bury the one true cause. [MED]
- assertsecrets/esoroundtrip.go:133 — spec.target.name is optional in the ExternalSecret API (ESO defaults it to metadata.name), but an empty value is a hard failure. With the default --all-namespaces scan, one third-party ExternalSecret written that way turns the fail-closed gate permanently red. [HIGH]
- tokeninv/inventory.go:56 — `linodeToken, _ := d.CloudToken()` drops the error; only best-effort path in the file emitting no ::warning::. A missing LINODE_API_TOKEN silently drops every Linode PAT from the inventory; the reconciler's SetGauge (no delete) then serves stale samples forever. [HIGH]
- tokeninv/rotationplan.go:130 — for lke-admin/db-admin the typed confirmation is built from the operator's own REGION input, so it can never catch a wrong region; `regions` emitted unvalidated even though in.Deployments is right there. A typo routes a real rotation at environment: infra-<typo>. [MED]
- assertsecrets/cobra_kick_harbor_provisioner.go:84 — kick-harbor-provisioner does three cluster mutations (delete job, create job --from=cronjob, annotate externalsecret --all-namespaces --all) through the raw exec seam with NO BINDING DECLARED, in the same package whose extension.go claims the grep for hidden writes found only the broad-PAT drill. [HIGH]

### C17 assertions/volumes + templatecommit
- volumes/assert_volume_encryption.go:227 — REGION_SHORT read from the runner env but set by NO workflow (only into the in-cluster llz-reconciler Deployment), so on every real run regionShort=="" and judgeVolume skips BOTH the dry-run-reap coupling check (written for the lke637974 15-volume leak) and the exact-label check. The gate ships with its two strongest legs dead; REGION is already in the job env and linode.RegionShort(REGION) is the needed value. [HIGH]
- volumes/assert_volume_encryption.go:265 — the read-only assertion builds its Linode client from cloudBinding("volume-tags") (cloud-MUTATE), so the declared assertion:verified[cluster-read, cloud-read] ceiling is never enforced at the wire. The assertion binding is never passed to capability.CloudFor anywhere in the package; extension_test.go only inspects the declaration, so the gap is invisible. [MED]
- volumes/reconcile_volume_tags.go:154 — UpdateVolume PUTs the label read at GET time; volume-tags and volume-labels are separate goroutines on the same PV watch, so a concurrent rename can be reverted to pvc-<uuid> and stay that way until the 3600s resync floor — while the assert gate's heal budget is 3 minutes. [LOW-MED]
- templatecommit: CLEAN.

### C18 assertions/assertplatform + assertreconciler
- assertplatform/healthworkflow.go:253 — healthWorkflowExpected calls clusterspec.LoadInstance(".") instead of the injected the LoadSpec field on Deps (wired to clusterspec.Detected(), which locates the spec root incl. instance-template/). The LoadSpec field is DEAD. Run from anywhere but the spec root and a missing WorkflowTemplate reads as "spec unreadable → skip, exit 0" — the exact unfalsifiable-gate hole the file's header says it fixed. [HIGH]
- assertplatform/deps.go:53 — `Install(d Deps) { deps = d }` replaces wholesale, so ExecCombined/Exec/LoadSpec become nil for any partial literal. Same defect assertreconciler documents as having reached e2e as a SIGSEGV and fixed by filling omitted fields (assertreconciler/deps.go:79 + TestInstallFillsOmittedSeams); capability_wiring_test.go only proves handles are live, not that func fields were set. [HIGH]
- assertreconciler/reconciler.go:181 — lane-freshness queries are unaggregated while the up/leader queries beside them use max(). promwire.VectorByLabel keys on `reconciler` alone, so duplicate series (rolling-update overlap) collapse last-wins and a dead pod's old timestamp can fail the gate. The LLZReconcilerStale rule this mirrors added `max by (reconciler)` for precisely this. [MED]
- assertplatform/healthworkflow.go:160 — prior-Workflow reap unconditionally refused: capability.Delete's allowlist rejects --field-selector (verified by running checkDeleteTargets directly), and the error is discarded via `_, _ =`. Its label selector (workflows.argoproj.io/workflow-template=) also doesn't match the label the submitted manifest carries (app.kubernetes.io/managed-by: llz-ci-assert). Untested path. [MED]
- assertreconciler/reconciler.go:319 — poll loop gates on the raw leader gauge; the authoritative Lease cross-check runs only after the loop exits, so a lagging gauge (the flake this was written for) costs the full 120s settle and ~9 port-forwards on a run that then passes. [LOW]

### C19 assertions/assertnetwork + sustain
- assertnetwork/enforcement.go:129 — evalEnforcementProbe treats ANY non-connected denied dial as "blocked". A DNS failure, exit code 2 (which resultFromExit's own doc calls "an inconclusive control"), or a missing container status all print `OK: netpol`. Loki is ManagedConditionalOn:"loki", so on a cluster without it the default denied target loki-gateway.monitoring:80 never resolves and the lane certifies CNI enforcement it never exercised. The vacuous pass the file's header exists to prevent. [HIGH]
- assertnetwork/enforcement.go:473 — leak-recovery deleteProbeNamespace uses --wait=false and the manifest is applied on the next line; after any --keep run or a killed process the apply lands in a Terminating namespace and fails. [MED]
- assertnetwork/enforcement.go:347 — waitProbePod waits for Succeeded first with the full budget though its own comment says Failed is the designed terminal phase. kubectl wait doesn't short-circuit, so every green run burns ~32s of dead time and a stuck pod costs 64s. [MED]
- assertnetwork/admission.go:229 — the `signature` check hard-fails when verify-llz-image-signature is absent, but imageSignature is ManagedConditionalOn:"kyverno" and deliberately not rendered for clusters that didn't opt in; the gating `surfaces` group reds on a correctly-rendered cluster. probePVCEnforcement already shows the Skipped shape this needs. [HIGH]
- assertnetwork/enforcement.go:180 — meshEnforcesSTRICT collapses the namespace default and all portLevelMtls entries into one boolean, so a STRICT mode on an unrelated port enables an assertion against port 80; the resulting FAIL text then blames the very portLevelMtls exemption the classifier discarded. [MED]
- sustain: CLEAN.

### C20 assertions/configreadiness + manifestguard
- configreadiness/preflight.go:65 — capacity-guard fallback reads `<linode-region>.tfvars` from CWD, but tfvars are named per DEPLOYMENT and live under terraform-iac-bootstrap/cluster/. In the delivered llz-terraform.yml path this read always fails, so clusterLabel/nodeType/nodeCount stay empty and the same-label orphan guard + vCPU quota guard are INERT on every adopter build — the exact inertness the workflow comment claims --deployment fixed. ResolveDeploymentScope already parsed those values and threw them away. [HIGH]
- configreadiness/cobra_preflight.go:36 — --vpc-limit/--vcpu-limit default to literal 0, not cli.EnvInt("PREFLIGHT_VPC_LIMIT"/"PREFLIGHT_VCPU_LIMIT", 0). The workflow exports both env vars with the comment "Read by llz as flag defaults (cli.EnvInt)"; no Go code reads them. Setting the repo variables leaves both quota guards report-only. [HIGH]
- configreadiness/readiness.go:229 — cross-file discriminator check reads cluster-bootstrap/<env>.tfvars, a root deleted with the cluster-bootstrap TF workspace. os.ReadFile errors → continue, so the deployment and apl_values_env consistency checks never run. Tests green only because readinessfixture_test.go synthesises that file. [HIGH]
- manifestguard/cobra_manifestguard.go:30 — argocd-rendered-apps joins --render-dir onto --root unconditionally, so an absolute --render-dir is cleaned into a relative path and the guard reports "no rendered manifests". PlaceholderGuardCmd handles and documents exactly this case ten lines below. [MED]
- manifestguard/argocd_apps_guard.go:54 — `if err != nil || len(files)==0` collapses a fence/read failure (ErrOutsideRepo, unreadable subtree) into the empty-corpus message, discarding both the error and the partial list CollectPaths deliberately returns. [MED]
- manifestguard/apl_schema.go:49 — unescapedPlaceholderRe's `(^|[^$])` prefix consumes the preceding byte and Go's FindAll is non-overlapping, so `${a}${b}` never matches `${b}`. An unwired placeholder adjacent to a wired one passes the var-contract check. Latent. [LOW]
- configreadiness/preflight.go:174 — --fail-on-orphans compared literally against "true"; "True"/"1" from a repo variable silently turn the orphan gate into report-only rather than erroring as invalid. [MED]

### C21 assertions/assertsuite+assertidentity+buildpreflight+seedspecial+assertregistry+reachability
- assertidentity/loginsmoke.go:331 — isDenied substring-matches "403" over the whole error text. Tested paths embed a 19-digit UnixNano suffix and transport errors carry the port-forward's ephemeral loopback port, so a 5xx or a dead port-forward is accepted as a permission denial and both out-of-subtree SECURITY assertions print "✓ correctly denied" having proved nothing. [HIGH]
- buildpreflight/preflight.go:309 — ghDefaultBranch/ghFileSHA reach GitHub via ghapi.GHAPIJSON → kubectlprobe.Exec, unconstrained by the binding's grants, while deps.go claims forgeHandle is "this package's ONE forge seam". Because ghapi sits in internal/shared, seambypass_test.go (which scans only internal/extensions) no longer counts it — the allowlist entry vanished without the bypass being removed. [MED]
- seedspecial/special.go:199 — a failed `kubectl get pvc` (or parse error) leaves rows nil and the audit prints "All PVCs are on block-storage-retain — Kyverno admission caught everything." An unread cluster is reported as fully encrypted at rest. [MED]
- assertregistry/roundtrip.go:74 — readSecretWithSettle hard-fails on any read error, so --settle absorbs only an ABSENT Secret. A transient API-server blip reds the lane on attempt 1, in the exact bootstrap window the settle loop was added for. [MED]
- reachability/verify.go:200 — knownHostsHas requires a line to start with `<host> `, so hashed (|1|…) and comma-aliased (host,1.2.3.4 …) known_hosts entries read as absent and make `llz verify` exit 1 on a config whose repo-server handshake check passes in the same run. [MED]
- assertsuite/suite.go:339 — a lane with an empty Steps slice is neither skipped nor failed, so it is counted in "All N gating lane(s) passed" — the vacuous-green shape the file's header exists to abolish. [LOW]
- assertsuite/delivered_lanes_test.go:66 — strings.Index(body, m[0]) re-finds the FIRST occurrence rather than the match's position; a second identical `assert-suite --only …` line makes the guard silently skip lanes. Use FindAllStringSubmatchIndex. [LOW]
- assertregistry/roundtrip.go:312 — --registry applied AFTER DecodeRobotSecret's UsableRegistryHost check, so the override cannot rescue the "harbor." truncation scar the lane was written for. [LOW]
- assertsuite/suite.go:557 — appendLaneSummaries ignores r.Skipped, so a skipped alert-eval still gets its "live rule evaluation" heading in $GITHUB_STEP_SUMMARY with the skip notice as its body. [LOW]

### C22 guards/sourceref + docsguard
- sourceref/adr.go:183 — `"-"+num[:0]` is the empty string, so the duplicate-ADR disambiguation exemption degenerates to strings.Contains(line,"-"). ANY line with a hyphen escapes the rule. LIVE PROOF: statepassphrase/plan.go:7 carries a bare `ADR 0007` (a duplicated number) and passes only because the line says `tf-encryption-env`. The existing test uses a hyphen-free sentence so it never catches it; the clause is redundant with the num+"-" check on the next line and should be deleted. [HIGH]
- docsguard/toc.go:81 — applyTOC anchors on firstH2Re, which ignores ``` fences (unlike docHeadings). REPRODUCED: `llz ci gen-toc` writes the whole TOC block INSIDE a ```bash fence when the first column-0 `##` is a shell comment, corrupting the file — and docs-guard's TOC check still passes because the entries resolve. [HIGH]
- docsguard/docsguard.go:912 — the platform.RenderTimeArtifact exemption is gated on `rendered` (instance-template only) and checkDeliveredDocLinks never consults it. REPRODUCED: a runbook or quickstart linking docs/README.md — the pointer deliver-docs itself writes — is reported dead twice. sourceref/guard.go applies the same set unconditionally; docs-guard is the inconsistent one. [MED]
- sourceref/symbols.go:266 — rep.IndexFindings is counted into the failure but the print loop only walks rep.Findings. An ADR index/directory disagreement fails CI with a bare count and no file, row, or reason, while the fully-formed message sits unused. [MED]
- docsguard/docsguard.go:388 — llzStartRe ends in `(\s+.*)$`, so only the first `llz` per line is ever matched. REPRODUCED: `llz ci gates --yes && llz ci gates --totally-bogus-two` yields no finding; the second invocation's flags are never validated, in a guard whose whole premise is that silent under-coverage is the defect. [HIGH]
- docsguard/docsguard.go:309 — shellVisible blanks quotes and substitutions but not `#` shell comments, and a `#` comment inside a run: scalar is part of that scalar. REPRODUCED: a run-block comment naming a retired flag produces a hard [flag] finding, contradicting workflowRunBody's stated design that comments stay free to describe history. [MED]

### C23 guards/budget + credcoverage
- credcoverage/extsecretpaths.go:66 — esRemoteRefRx requires `key:` on the line immediately after `remoteRef:`; a comment or any other field in between makes the ExternalSecret invisible to the whole cross-validation. LIVE IN THE REPO TODAY: platform-apl/components/cidrFirewall/llz-cidr-firewall/externalsecret.yaml:54 has three comment lines there; verified the regex matches nothing for that shape. Masked only because another manifest references the same key. [HIGH]
- budget/count.go:46 — `indent := len(m[1])` ignores the `- ` the regex itself accepts, so for a `- run: |` step the sibling keys (shell:, if:) are counted as run-body shell. Measured: 4 instead of 2 on a two-step sample. Bites composite actions, where shell: is mandatory. [MED]
- credcoverage/extsecretpaths.go:242 — esSeedTableEntryRx's lazy `(?s).*?` span makes a fieldSpecs-less table entry swallow the following entry: measured paths={a/one} with b/two erased. The gate would accuse a manifest of an unseeded path that is in fact seeded. [MED]
- credcoverage/coverage.go:276 — WalkDir callback errors are dropped, so a partially-broken walk still passes RequireCorpus and vouches for workflows it never read. The sibling verb in the same package documents and fixes exactly this. [MED]
- credcoverage/extsecretpaths.go:254 — collectSeededGo silently skips a missing Go seed source, so renaming any of the nine hardcoded seeder paths quietly removes its paths from the policy check. No corpus guard on that list. [MED]
- credcoverage/coverage.go:161 — credSecretRef is uppercase-only; GitHub secret names are case-insensitive, so `${{ secrets.harbor_admin_password }}` would be an unmeasured credential the guard reports as covered. [MED]
- budget/cobra_core_surface.go:34 — user-facing help text reads "Never raise it to go color.Green." A mechanical green→color.Green rename also damaged comments in credcoverage/coverage.go:39,203 and three test files. [LOW]
- CROSS-CUTTING: tools/internal/shared/pathglob/pathglob.go caches compiled regexps in an UNSYNCHRONISED package-level map. Harmless while gates run sequentially; a data race the moment any caller runs two gates concurrently. [MED]

### C24 guards/plaintext + wavehealth + chartguard
- plaintext/guard.go:949 — `ind <= keyIndent` ends the endpoint list on the first item when a YAML sequence is written flush with its parent key. The whole "scrape with no scheme:" detection is blind to that style; every test fixture uses the indented style. Probe: indented → 1 finding, flush → nil. [HIGH]
- plaintext/guard.go:400 — reInsecureCLI's curl arm requires whitespace immediately before `-k`, so `curl -sk` and `curl --insecure` bypass it. Probe confirms both return nil; only bare `curl -k` fires. [HIGH]
- wavehealth/health.go:282 — the Argo health-override cross-check is strings.Contains(values, key+":"), so a COMMENTED-OUT override in apl-values/values.yaml counts as present. Probe confirms allowed=true for a wave-5 NetworkPolicy whose override is only in a comment — re-opens the PR #142 wedge silently. [HIGH]
- chartguard/version.go:143 — classifyChartBump only rejects newVer==oldVer, so a DOWNGRADE/revert to an already-published version passes as OK; publish-charts won't push it and clusters keep the pre-revert artifact under a matching pin. [HIGH]
- plaintext/guard.go:756 — locator() returns the bare port for five distinct finding kinds, so a defaulted-scheme finding and an insecureSkipVerify finding on port `metrics` in one file share the key file:metrics. Registering one silently vouches for the other, violating locator's own stated contract. [MED]
- chartguard/pin.go:207 — a pin naming a first-party chart that no longer exists locally (rename/delete) is skipped as "third-party". `checked` merely drops from 3 to 2 and the guard passes — the 404-at-sync-time failure it exists to prevent. [MED]
- wavehealth/dependency.go:243 — imagePullSecrets and volumes[].projected.sources[].secret are not modelled, so those hard Secret dependencies escape the #163 gate. Latent. [LOW]
- wavehealth/dependency.go:137 — RequireCorpus counts files, not compared pairs. The guard evaluates only 9 refs / 3 workloads today; if the decode structs stop matching it prints green having compared nothing. Both sibling guards refuse that shape explicitly (pin.go:117, lock.go:198). [MED]

### C25 guards (11 small packages)
- callerperms/guard.go:174 — check only compares scopes the callee needs at `write`, so a caller job whose explicit permissions: block omits a `read` scope the callee declares passes green while GitHub startup_failures the run. The guard's own remediation text names this exact case. [HIGH]
- monitoringlabel/guard.go:65 — DEMONSTRATED LIVE: with rendered/ absent, `llz ci monitoring-label-guard --root ../platform-apl --root ../rendered` exits 0 with "all … carry prometheus: system". RequireCorpus counts across all roots COMBINED, so the openbao ServiceMonitor (only resolvable after make render-charts) goes unchecked. mesh-egress-guard has an explicit requireRenderedCharts; this one does not, despite guardkit's header claiming it fails closed here. [HIGH]
- mtlsguard/guard.go:109 — podTemplate() handles Deployment/StatefulSet/DaemonSet/CronJob but NOT Job/Pod, and only spec.containers is walked (no initContainers). A Job or init container declaring OPENBAO_ADDR without the mTLS mounts is silently blessed, contradicting the "no allowlist — new consumers inherit it automatically" claim. [HIGH]
- workflowshells/guard.go:106 — `wfBash || jobDefault` INVERTS GitHub's precedence: a job-level defaults.run.shell OVERRIDES the workflow-level one. A container job that sets shell: sh under a bash-defaulted workflow passes while its `set -o pipefail` steps run under dash. [HIGH]
- monitoringlabel/cobra_guard.go:24 — --root is a list of scan dirs, but registry/gates.go (the only runner) passes the repo root on --root, so the documented default is dead and the guard walks the whole checkout — including testdata/, which guardwalk (unlike the versionpins/setupgosite walkers) does not skip. [MED]
- versionpins/pins.go:318 — maskComments blanks only whole-line comments, so a trailing `# … ci-tofu:1.9.8` comment is scanned as a live pin and fails the build pointing at prose — the exact thing the function's header says it prevents. [MED]
- coverageguard/guard.go:185 — coverageForSuffix returns the first sorted match and never reports a second, so two packages sharing a path tail leave one silently ungated. [MED]
- coverageguard/bank.go:147 — `--bank` prints every planned floor as raised even when applyBank matched fewer lines (e.g. a non-integer floor covMinLine cannot match); with n>0 the error branch is skipped and the operator commits believing a floor was ratcheted. [MED]

### C26 shared/clusterspec
- overlay.go:179 — RenderAppsOverlayEnv ignores ManagedSkip/EmitOnManaged: it emits `gitea: enabled: false` (which the reconciler commits onto apl-core's own env/apps/gitea.yaml, DISABLING the values-repo backend managed apl-core runs) and force-enables trivy/policy-reporter/kyverno on every managed cluster. Confirmed by probe. [CRITICAL]
- kustomize.go:492 — clusterHealthWorkflow's llz image is never retagged; the cited retagHealth JSON6902 mechanism does not exist anywhere, and llzimage_test.go:34 hard-exempts the component on that belief, so its WorkflowTemplate ships mutable :latest. [HIGH]
- merge.go:40 — mergeCluster never inherits nodePool.autoscalerMin/autoscalerMax from spec.defaults; autoscalerEnabled IS inherited, so the cluster autoscales on the TF root's 3/6 defaults instead of the declared range. Confirmed by probe; contradicts docs/landing-zone-spec.md:334. [HIGH]
- aplversion.go:136 — aplChartVersionError and AplChartVersionWarnings have NO production callers. The major-ahead block does not exist (aplChartVersion: 7.0.0 clears the >=6.0.0 floor and deploys), and the minor-drift warning that two code comments and a design doc advertise is never printed. [HIGH]
- components.go:83 — DependsOn is enforced over toggles only, never over the managed emission gate: an explicit managedApps without loki drops observability while llzReconciler still emits its ServiceMonitor/PrometheusRule against absent CRDs. [MED]
- defaults.go:36 — an explicit `managedApps: []` is indistinguishable from unset and gets overwritten with DefaultManagedApps, contradicting the adjacent comment. [MED]

### C27 shared/capability
- repowrite.go:105 — resolveForWrite resolves only the target's PARENT, so a symlink at the LEAF component is followed. With `root/out -> /etc/passwd`, WriteFile("out",…) returns nil and OVERWRITES THE OUTSIDE FILE. Repo.Resolve refuses the same path, so the reader is strictly stronger than the writer, and the file header's claim that the escape is closed does not hold. TestAWriteCannotEscapeThroughASymlinkedParent covers only the parent case. [CRITICAL]
- forge.go:169 — classifyAPIMethod misses pflag's ATTACHED shorthand. `gh api -XDELETE repos/o/r` classifies as `read` and a cloud-read handle permits it; `gh api … -ftitle=x` likewise classifies as read while gh sends POST. `-X=DELETE` is handled and tested; `-XDELETE` is not. [CRITICAL]
- repo.go:323 — RepoContainingAll's absolute branch inspects only dirs[0] and returns a one-element slice, silently dropping every other scan root. monitoring-label-guard with absolute --root values scans half its corpus and prints all-clear. [HIGH]
- repo.go:337 — when filepath.Rel fails the dir is handed back verbatim, and the comment asserts the fence "refuses it loudly". It does not: with ["../platform-apl","rendered"] the root becomes `..` and `rendered` silently resolves to `../rendered`. [HIGH]
- cloud.go:234 — CloudFor reads b.Grants directly and skips the Gate/Assertion blanket rules For() applies, so an Assertion binding gets a MUTATING Linode client (Permits("DELETE")==nil) where For(b).Cloud refuses. RepoWriterAt (repowrite.go:207) has the same gap. ~30 non-test sites call CloudFor directly. [HIGH]
- kubeapi.go:63 — KubeFor likewise ignores the binding kind: an Assertion with cluster-write gets a refusing Writer but a live MergePatch on the in-cluster transport — which this file calls "the code that least deserves the gap". [HIGH]
- capability.go:475 — WithExec claims to replace the process seam "for tests that must not shell out" but rewires only Cluster and Writer; Secrets, Custodian, BaoAdmin, Forge and Cloud keep their real bao/gh/HTTP seams. [MED]
- THEME: the two highest-value bugs are re-runs of failure classes this package's own headers document as already fixed, one component/spelling further out. The kind-blanket-rule gap is a boundary problem — enforced in For() and in NONE of the four sibling constructors that bypass it.

### C28 shared/extension
- registry/gates.go:623 — GatesCmd resolves the gate tree via repoRoot (walk up for .git) but resolves enablement via clusterspec.Detected(), which is CWD-relative. The Makefile's LLZ_CI macro cds into tools/ first, so `toggles` is always nil and RunGates' entire skip path is DEAD; and `--root <other repo>` applies the CWD repo's toggles to a different tree. [HIGH]
- registry/commands.go:93 — Command.Extension is documented as an extension name but holds Go PACKAGE names ("assertidentity","budget","credrotate"). Verified: registry.Lookup fails on every row. Gate.Extension and assertsuite.Step.Extension use real extension names, so the two namespaces silently don't join — which is why nothing can check that a battery step names the extension that actually owns its verb. [HIGH]
- validate.go:251 — the `continue`s on unknown kind/state skip the binding-name format check, checkRequires, and the duplicate-binding bookkeeping, contradicting the docstring's "reports every problem, not just the first". [MED]
- doc_agreement_test.go:261 — off-by-one: strings.Index(section[1:], …) indexed against section[1:] but sliced as section[:j], truncating the Grants section one byte early; a doc reflow ending the section on a grant name turns the guard spuriously red. Should be section[:j+1]. [MED]
- registry/enablement_test.go:65 — `if !e.Extension.Always && !e.Enabled` makes the only both-directions enablement assertion self-disable if obj-encryption ever ships Always:true. [MED]
- registry/gates.go:450 — Run.Root is documented "Required" but unvalidated; a non-CLI caller omitting it invokes every gate with --root "". [LOW]

### C29 shared/health + linode
- health/argo.go:169 — a sync operation with phase Failed/Error lands in OpErr, which classifyArgoApp consults ONLY via IsAnnotationLimitError. Any other failed-sync message is discarded, and an OutOfSync/Healthy app then falls to CatDrift — "drift only; workload functional" — which Verdict() ignores. This is exactly the incident the SyncErr path documents at argo.go:313-324, but that path is guarded on phase "Running" only. [HIGH]
- linode/linode.go:127 — ListRaw has NO PAGINATION (no page/page_size, never reads `pages`), so it returns only the first 100 items. Its production caller lists object-storage/buckets for the brownfield report on a shared account. FindIDByLabel in the same package documents this exact hazard. [HIGH]
- health/pod.go:79 — PodIsFailing/ReadyRatio read only containerStatuses, so a crashlooping NATIVE SIDECAR (restartable init container) on a phase-Running pod reads as healthy 1/1. PodIsStarting, PodIsWarmingUp and FlappingContainers all do read initContainerStatuses. [HIGH]
- health/sla.go:155 — PATOverPolicy is decided from daysLeft (time REMAINING) but documented as "lifetime exceeds the max-days policy". A PAT minted with a 2-year expiry fails the gate at first, then silently PASSES for its last maxDays — going green because the violation was ignored long enough. [HIGH]
- linode/rotate.go:101 — deleteExpect2xx treats 404 as an error, unlike DeleteResourcePath/DetachVolume/UpdateVolumeLabel in the same package. Re-revoking an already-revoked PAT or OBJ key reds the rotation job on a no-op. [MED]
- health/infra.go:102 — ClassifyLeaderCount(0,0) returns CatOK with the message "exactly one active OpenBao leader". If pod enumeration comes back empty, the OpenBao HA section prints nothing and the gate advances. [HIGH]
- health/foundations.go:267 — the SC ownership-tag audit uses LKEIDFromTags (^lke-?[0-9]+$) and so blesses `lke-<id>`, but teardown tracks with exact `t == "lke"+clusterID`. The audit's stated "iff reap can actually attribute it" contract fails for the dashed form. [MED]
- linode/reap.go:306 — DetachVolume returns nil for EVERY 400, assuming "already detached", so any other 400 is reported as a successful detach and the loop re-polls to its deadline with no diagnostic. [MED]

### C30 shared/openbao + baoread + forge + tfroots
- openbao/openbao.go:497 — Rollback never checks the restore POST's status code. A 403/429/500 returns nil, so DualWrite prints "primary rolled back to vN" while the primary still holds the NEW credential and the secondary the OLD one. TestRollbackRestoreTransportFailure guards exactly this outcome for transport errors; the HTTP-status path is unguarded. [CRITICAL]
- openbao/openbao.go:483 — the prior-version GET is also status-unchecked. An error body decodes cleanly into kvResponse, leaving kv.Data.Data == nil, and the restore then POSTs {"data":null} OVER A LIVE SECRET. [CRITICAL]
- baoread/exec.go:187 — WaitForState accumulates its budget in `interval` units, but each probe goes through ExecFn, which can block ~296s on transient retries. A nominal 10m leader wait can run for HOURS during the exact cold-bootstrap konnectivity flap the retry budget was widened for, and the job dies before the diagnostics print. [HIGH]
- tfroots/roots/cluster/main.tf:43 — data.linode_vpcs.shared[0].vpcs[0].id filters on LABEL ONLY and takes element 0. vpc_label carries no instance prefix (unlike object-storage/databases label_prefix), so two instances on one account can match each other's VPC; zero matches give an opaque "Invalid index" at plan. [HIGH]
- baoread/exec.go:205 — DumpDiagnostics uses the retrying ExecFn rather than ExecPod, so diagnosing an already-timed-out unreachable pod burns another ~5 min before printing anything. [MED]
- forge/gitlab_capabilities.go:228 — expiresAt truncates now+ttl to a DATE while TokenExpiry parses it back as midnight UTC, so any sub-day TTL round-trips to an expiry IN THE PAST. Latent (forge.Supported still rejects gitlab). [LOW]

### C31 shared/keycloak + kube + instanceresolve + apl
- apl/overlay/overlay.go:112 — the `len(bytes.TrimSpace(objMerged)) > 0` guard is DEAD: clusterspec.MergeOverlay(nil,nil) returns "{}\n", so with no obj overlay source on the source branch the reconciler pushes env/settings/obj.yaml = {} onto the machine branch, WIPING apl-core's AplObjectStorage CR. Proven with a throwaway probe against the package's own fakeRepo. Root cause is readMergedOverlay discarding both `found` flags. Existing tests miss it because the one no-obj-source case uses credsMissing, which takes the skip branch. [CRITICAL]
- keycloak/smokeops.go:78 — EnsureDirectGrantClient creates the client, then attaches the scope/audience mapper, returning (uuid, err) with a LIVE uuid on post-create failure. The caller (loginsmoke.go:128) registers `defer DeleteClient(clientUUID)` AFTER the error check, so an unconverged openid scope leaks a PUBLIC ROPC CLIENT per failed run — the exact exposure DeleteClient's own comment says must never look clean. [HIGH]
- instanceresolve/objcluster_resolve.go:128 — objClustersInRegion returns ok=true for a successful-but-EMPTY listing, so `case 0` hard-fails `llz env add` with "this account has no object-storage cluster in region X" when the real cause is an under-scoped PAT. Contradicts AccountRegions, which explicitly treats an empty listing as unknown. [MED]
- instanceresolve/region_resolve.go:75 — AccountRegions returns ok=false via len(ids)>0 on a path that never calls reportSkippedAccountCheck, so a listing that parses to zero ids silently skips the check — the silence account_check_skip.go exists to eliminate. [MED]
- kube: CLEAN. apl/identity: CLEAN.
- Cosmetic: kube/secretprobe.go:110 dangling maskGHALines comment; kube/secret.go:21 + secretprobe.go:58-75 duplicated/garbled doc fragments.

### C32 shared/terraform+manifest+kubectlprobe+ghgitdata+ghsecret+objstore+envreq+tokenprobe
- terraform/tfvars.go:124 — splitAssign strips a trailing comment only for QUOTED values. `node_count = 5  # ...` (valid HCL) makes Atoi fail silently → NodeCount=0 → converge/wait.go:41 falls back to --expect-nodes default 1, so wait-cluster-ready PASSES WITH 1/5 NODES READY. The doc comment promises comments are ignored. [CRITICAL]
- objstore/objstore.go:214 — SampleObjectKeys never XML-unescapes <Key>. A key `a&b` is returned as literal `a&amp;b`; teardown's s3DeleteObjects then xml.Marshals it to `a&amp;amp;b` and deletes nothing → drain stalls 5 rounds → teardown aborts. objenc's SSE-C probe HEADs the wrong key and reports absent. [HIGH]
- tokenprobe/s3probe.go:113 — a revoked state-bucket key (403 InvalidAccessKeyId, empty HEAD body, no x-amz-error-code on Ceph/RGW) falls through to the StatusForbidden arm and is reported VWarn "valid but not authorized" — the exact failure the probe exists to catch. [HIGH]
- kubectlprobe/probe.go:165 — `var verdict Verdict` zero-value is Found; with Retries<=0 the loop never runs and Probe returns (nil, Found). ExistsOK then reports exists=true WITHOUT EVER INVOKING KUBECTL. converge already sets Retries=1, one decrement away. [HIGH]
- envreq/envreq.go:212 — `gh api` without --paginate/per_page: repos with >30 Actions variables or env secrets silently truncate at page 1, so `llz doctor` reports configured REQUIRED credentials as missing and the wizard re-pushes them. [MED]
- objstore/objstore.go:302 — S3ObjectRequest signs the raw key instead of routing through S3EscapePath (which sibling S3SignedRequest uses 200 lines above). Latent today; a key with a space or `+` yields SignatureDoesNotMatch, which ExplainS3Write misreports as "the credential has been revoked or rotated". [MED]

### C33 shared/yamledit+harborauth+ghapi+envtopology+envdef+metrics+cigate+cli+guardwalk+validate+llzver+credtargets+answers+proc+copier+ghcli
- NOTE: the `dispatchwatch` package did NOT EXIST at origin/main (it is new on the current feature branch) — not reviewable in this sweep.
- yamledit/yamledit.go:157 — SetSpecPath never checks the first childMapping(doc.Content[0],"spec"); a spec: key that is null/scalar/sequence (or a non-mapping root) PANICS with a nil-pointer deref instead of the "crosses a non-mapping" error the function already defines. Reproduced (SIGSEGV at yamledit.go:172). Reachable from `llz spec set`, `llz env set`, `llz network add`. [HIGH]
- envtopology/topology.go:72 — with a spec present, ReadTopology returns spec envs only, while ListDeployments in the same file returns the UNION of spec + cluster/*.tfvars. A tfvars-only deployment is in the CI matrix (llz env list --json) but `llz env resolve <name>` errors "no such Deployment", failing the OpenBao bootstrap step for that env. [HIGH]
- yamledit/yamledit.go:129 — EditYAMLFileVia reads with yaml.Unmarshal (doc 1 only) and rewrites the whole file, SILENTLY DELETING any document after the first; the strict re-parse passes on the truncated bytes so the rollback never fires. Reproduced. [HIGH]
- ghcli/ghcli.go:51 — Quote's needs-quoting char set omits backtick, backslash, newline and glob metacharacters, so tokens containing them print bare; the openbao copy of this function even documents backtick protection as its rationale while sharing the gap. [MED]
- yamledit/yamledit.go:207 — inferScalarTag classifies leading-zero values with base-10 Atoi but writes them as !!int, and yaml.v3 re-reads them as OCTAL (0755 → 493). Reproduced. [MED]
- NOTED not filed: envdef.EnsureLandingZone (envdef.go:140) writes landingzone.yaml with a raw os.WriteFile, bypassing the capability fence, two lines before add.go routes the sibling write through capability.RepoWriterAt. Declaration honest today, but the same "write laundered through a shared package" shape the neighbouring comments say was fixed.

### C34 verbs/upgrade + selfupgrade
- cobra_upgrade_test_gate.go:96 — --dir default is `.Upgrade-test` while .gitignore ignores `/.upgrade-test/`; a RENAME REGRESSION from commit f265af67, so on Linux the gate's build tree is untracked-but-not-ignored inside the template checkout (and the flag help still says "(gitignored)"). [HIGH]
- cobra_upgrade_test_gate.go:405 — `--dir ""` (or `--dir .`) makes build==root, so os.RemoveAll(build) DELETES THE ENTIRE TEMPLATE CHECKOUT. [HIGH]
- selfupgrade/upgrade_policy.go:51 — SnapshotUpgradeOwned returns the zero UpgradeSnapshot on a copy failure, discarding s.dir; the already-populated temp dir of owned files is never cleaned up. [MED]
- cobra_upgrade_test_gate.go:689 — assertTasksRan's message says "This is a harness failure, not a finding" and blames the --llz binary, but runUpgradeHop:572 reports it as the tasks-delivered CHECK failure — the exact misdirection the comment at 564-571 claims to have fixed. [MED]
- upgrade.go:286 — conflictFiles scans all unstaged + untracked files rather than what copier changed, so a pre-existing conflict marker aborts an already-mutated tree with a diagnosis blaming copier. [MED]
- The same rename commit mangled a dozen comment/message strings in these packages (color.Green gate, cli.Prompt, color.Red for a reason, target template Version). [COSMETIC]

### C35 verbs/newinstance + onboard
- onboard/tokens.go:106 — `llz tokens` probes cached credentials, PRINTS "invalid — rotate it", THEN PUSHES THEM ANYWAY. pushToRepo writes every entry of the .llz/secrets.env map unconditionally, so a dead cached PAT overwrites a good live repo secret; have() is presence-based so the wizard never re-prompts it and its own "re-run" remediation can't converge. [HIGH]
- onboard/tokens.go:134 — have() → envreq.Satisfied tests key PRESENCE, not a non-empty value. `LINODE_API_TOKEN=` (the natural way to force a re-prompt) reads as satisfied, is skipped, and is then pushed via proc.Run(argv,""), which wires os.Stdin — so `gh secret set` SWALLOWS THE OPERATOR'S NEXT TERMINAL LINE as the secret value. [HIGH]
- onboard/wizard.go:373 — PushSecrets emits argv with no --repo (gh resolves from cwd remotes), while its passphrase-clobber guard and branchpolicy.Lock both resolve instance_repo from .copier-answers.yml. If those differ, the guard clears one repo and the write lands on another — exactly the state-destroying overwrite the guard exists to prevent. [MED/PLAUSIBLE]
- onboard/wizard.go:401 — it.argv[2] is the literal "set", not the credential name (SecretSetArgv/VariableSetArgv put the name at index 3). Every push failure reports `set: exit status 1`; the sibling loop in tokens.go:569 gets this right. [LOW]
- onboard/tokens.go:110 — --dry-run still MkdirAll(".llz") (:66) and rewrites .llz/vars.env on the "nothing to do" path, both before the dry-run bail at :114. [LOW]
- newinstance/new.go:374 — `llz new --push --dry-run` reports "instance_repo is still the placeholder" because answers.Read returns (nil,nil) for a scaffold copier never wrote; absent-file and placeholder collapsed into one branch. [LOW]

### C36 verbs/lint + mutate
- mutate/runcmd.go:53 — gremlins invoked with `--timeout-o.Coefficient`; A FIELD RENAME RAN THROUGH THE STRING LITERAL. gremlins rejects the unknown flag, produces no mutant lines, and every `llz ci mutate` dies in harness validation with "no mutants were reported at all". The package's tests only exercise parseGremlins/validateRun on canned output, so nothing ever asserted the argv. (Already fixed on unmerged branch fix/coverage-ratchet-gaps as d3f359ce.) [CRITICAL]
- mutate/runcmd.go:56 — same corruption gives `--o.Integration` instead of `--integration`; that flag is the fix for gremlins' false-100% on package main, so fixing only :53 restores a FLATTERING score rather than a working gate. [CRITICAL]
- lint/lint.go:245 — stepGoFmt discards cigate.RunCombined's ok bool and parses combined stdout+stderr as the file list: a gofmt that resolves on PATH but fails to run returns empty output and THE GATE PASSES FOREVER; a gofmt parse error is misreported as a file "needing formatting" with unusable `gofmt -w` advice. [HIGH]
- lint/hooks.go:110 — --diff-filter=ACMR excludes DELETIONS and the empty staged list returns early, so a deletion-only commit runs NO LINT AT ALL; `git rm` of a template-owned managed file sails past stepVendoredFresh. [HIGH]
- lint/hooks.go:88 — os.WriteFile only applies perms on create, so re-arming over a pre-existing 0644 .git/hooks/pre-commit leaves it NON-EXECUTABLE while printing "armed pre-commit hook"; git silently ignores it. Verified empirically. Needs explicit os.Chmod. [HIGH]
- lint/lint.go:320 — actions-lint globs only .github/workflows/*.yml, so .yaml workflows are silently unlinted, and in a .yaml-only checkout --strict hard-fails with a false "found nothing to examine". [MED]
- mutate/run.go:180 — a `red(`→`color.Red(` sed ran through STRING LITERALS: user-facing output and --help say "the suite is color.Red/color.Green on unmutated source" (also run.go:165, cobra_mutate.go:23, comments at run.go:27, runcmd.go:49, lint.go:223). The same sed hit shared/cigate/cigate.go:219 ("the empty kubeconfig color.Red herring"). WORTH A REPO-WIDE GREP. [MED]

### C37 verbs/argodiag + recondiag + doctor + phasetiming
- phasetiming/phase_timing.go:229 — execCombined delegates to kubectlprobe.Exec (stdout-only, error dropped) despite its name/doc; a failing `kubectl logs` writes a 0-byte apl-operator.log into the uploaded timing bundle with no reason recorded. kubectlprobe.Combined exists for exactly this and is already imported. [MED]
- argodiag/moved_test.go:23 — TestDiagnoseArgoCD sets a bogus KUBECONFIG but never fences $HOME, and kubectlprobe.Exec is still real at that point; on a dev machine with a working ~/.kube/config THE TEST SHELLS OUT TO THE LIVE CLUSTER and fails. recondiag's equivalent test does set HOME. [MED]
- recondiag/diagnose.go:84 — the probe labelled "llz_apl_overlay_synced — the gauge converge's message names" runs `kubectl get servicemonitor,service`, NEVER READING THE GAUGE. The stated reason the verb exists is unmet, and the comment claims otherwise. [MED]
- argodiag/diagnose.go:63 — the node sweep prints only lines starting with `Conditions:`/`Allocated resources:`, which are SECTION HEADERS; the actual `Ready False KubeletNotReady …` rows are dropped, so the closing hint "check Nodes / Taints / Conditions above" points at empty labels. [MED]
- doctor/linode.go:143 — SpecK8sVersions falls through to the all-envs loop when the named env is absent, so `llz doctor --env stg` on a spec without stg prints green version checks for every OTHER env. [MED]
- doctor/deps.go:103 — firstNonEmpty trims to test emptiness but returns the UNTRIMMED value; a padded TF_STATE_ENDPOINT or LINODE_TOKEN turns a valid credential into a false "unreachable"/"check the PAT scope" advisory. [MED]
- phasetiming/phase_timing.go:87 — a single-mark log yields a nil interval slice, so phase-timeline.json is written as the literal `null` rather than `[]`. [LOW]
- phasetiming/phase_timing.go:209 — appendGHAFile duplicates ghaout.Append, which the sibling file in the same package already uses. [LOW]

### C38 tools/internal/cli
- ci.go:83 — installConvergeDeps(cliopts.Global) runs at tree-construction time, BEFORE cobra parses --dry-run, so converge.Deps.DryRun is permanently false. EMPIRICALLY CONFIRMED: built the binary, ran `llz --dry-run ci nudge-argo` with a stub kubectl on PATH — it issued real `annotate … refresh=hard --overwrite` and `patch … {"sync":{}}` writes against both Argo Applications. The comment right above the call claims the placement exists BECAUSE the globals are parsed by then; they are not. Fix pattern already in the tree at clusteraccess/cobra_cluster_access.go:38. [CRITICAL]
- assertsuite_enablement.go:32 — lane-skip enablement resolves from spec.defaults.components, but component toggles are PER-ENVIRONMENT (`llz apl app enable --env prod` writes only environments/prod.yaml, and mergeComponents makes env win). A component enabled only in the target env makes every one of its assert lanes report SKIPPED — a green battery over an unverified component, the exact hazard the file's own doc comment forbids. [HIGH]
- apl_app.go:82 — --dry-run still WRITES environments/<env>.yaml (yamledit.EditSpecFile has no dry-run notion) and then dry-runs the render, half-applying the toggle while telling the operator nothing changed. `llz env set` shares this shape, so the fix likely belongs at the yamledit/Deps layer. [HIGH]
- envtopology.go:40 — Deps.DryRun captured at init(); unread inside the environments package today, but the same defect as the first finding waiting for its first reader. [LOW]

### C39 tools/cmd + go.mod/go.sum
- CLEAN: go build passes; go mod tidy -diff no drift; go 1.25.0 matches CI and the Dockerfile; govulncheck no called vulnerability in any required module.
- dockerfiles/Dockerfile:47 — `-X main.version=${LLZ_VERSION}` names a symbol that NO LONGER EXISTS (the stamp moved to internal/cli.Version). The linker silently drops it, so every image-baked llz reports "dev", and `llz ci assert-image-fresh` takes its unstamped warn-and-skip path on every instance pipeline — the image/template skew guard is PERMANENTLY INERT. version_stamp_test.go only scans llz-release.yml, so nothing catches this. [HIGH] (matches the known ldflag-X-silent-ignore scar)
- tools/cmd/llz/main.go:17 — package doc claims cmd_llz_imports_test.go "refuses any import here but internal/cli"; that file exists NOWHERE. Real enforcement is the 6-line budget plus a discriminator test in another package, and an import SWAP would pass both. [MED]
- tools/cmd/llz/main.go:4 — doc says llz orchestrates "the repo's scripts/*.sh"; there is no scripts/ directory. source-ref-guard only scans tools/... literals so it never fires. [LOW]

### C40 terraform-modules
- VERIFIED: dumped the linode provider schema and confirmed every attribute used exists with the assumed type/optionality; ran tofu validate on all three modules (clean). Four candidate findings killed by those checks.
- llz-cluster/main.tf:70 — control-plane ACL hardcoded `enabled = true` (= default DENY) while control_plane_acl_ipv4/ipv6 and github_runner_*_cidrs all default to []; the module's own README "Minimal" example sets none, so the cluster comes up WITH NOBODY ALLOWED TO REACH THE API SERVER — and `ignore_changes = [control_plane[0].acl]` means adding CIDRs and re-applying can never fix it. [CRITICAL]
- llz-cluster/main.tf:30 — time_sleep.vpc_settle has no `triggers`, so on any VPC replacement (e.g. a region change) it plans as a no-op and the subnet create fires with zero delay, re-exposing the transient [403] Unauthorized the resource exists to prevent. [HIGH]
- examples/cluster-bootstrap.yml:46 — the copy-me example uses ./.github/actions/setup-terraform, which exists in neither this repo nor instance-template/ (the shipped one is terraform-init); the run dies at step 2. [HIGH]
- examples/cluster-bootstrap.yml:91 — two steps read `terraform output -raw argocd_deploy_public_key`, an output that exists nowhere and that the modules' README says was deliberately removed. [MED]
- llz-databases/main.tf:7 — main.tf and all five credential outputs document secret/platform/db-admin/<name>; the CODE WRITES secret/infra/db-admin/<name>, and secret/platform is precisely the default-team-writable path abandoned for that reason (clusterspec/validate.go:105-115). The module's own README already says secret/infra. [HIGH]
- llz-databases/variables.tf:71 — public_access = true is offered for a migration window, but the module never sets allow_list and exposes no input for it, so the public endpoint still admits no source. [MED]
- llz-cluster/README.md:133 — Inputs table omits apl_enabled, which RELEASING.md makes the SemVer surface; the flag is ForceNew, so discovering it late means RECREATING THE CLUSTER. [MED]
- llz-object-storage/outputs.tf:16 — description claims buckets are pinned "via endpoint_type"; they are pinned via s3_endpoint and endpoint_type is never set (same stale phrasing in variables.tf:15). [LOW]

### C41 kubernetes-charts
- VERIFIED: rendered every chart with helm template and cross-checked the vendored openbao-0.13.0 subchart.
- llz-cert-automation/templates/rbac.yaml:23 — the runner SA gets only `get` on the haproxy-tls Secret; NOTHING grants `create` on workflows.argoproj.io (the Sensor's submit) or create/patch on workflowtaskresults (needed by every workflow pod on a non-default SA). THE ENTIRE CERT-RENEWAL FLOW IS NON-FUNCTIONAL. The sibling clusterHealthWorkflow/rbac.yaml hit and documented the exact same executor failure. [CRITICAL]
- llz-cert-automation/templates/haproxy-rebuild-workflowtemplate.yaml:112 — the buildah image ref is built from harborUrl, which is defaulted and documented as a full URL WITH SCHEME, so IMAGE=https://harbor…:5000/platform/haproxy:tag is an invalid reference. The chart's own ESO template is careful to use the scheme-stripped registry_host. [HIGH]
- llz-cluster-foundation/templates/coredns-restart-job.yaml:76 — the Role scopes list/watch with resourceNames, which RBAC NEVER MATCHES for collection requests, so `kubectl rollout status`/`wait` are forbidden and the PostSync hook fails after the restart already happened. [PLAUSIBLE/HIGH]
- llz-openbao-platform/values.yaml:412 — raft retry_join hardcodes .llz-openbao.svc.cluster.local while the cert SANs and everything else derive from .Release.Namespace. Verified by rendering into vault-prod: RAFT CAN NEVER FORM. Real trap for the "sibling deployment" use case the chart is built for. [HIGH]
- llz-openbao-platform/templates/openbao-servicemonitor.yaml:16 — the selector app.kubernetes.io/name: openbao matches 4 Services (server, -internal, -active, -standby), all with a port named https, so each pod is scraped 3× and every vault_* series is TRIPLICATED — aggregations and alerts over-count. Needs component: server. [HIGH]
- llz-cluster-foundation/templates/_helpers.tpl:58 — the sc-patcher pre-check only tests that desiredDefault EXISTS, not that it is annotated default; if it isn't, the demote still runs and leaves the cluster with ZERO default StorageClasses — the exact outcome the guard's error message claims to prevent. [HIGH]
- llz-argo-bootstrap-apps/templates/applications.yaml:29 — `{{- with .revisionHistoryLimit }}` drops the value 0, which the schema explicitly permits; Argo silently applies its default of 10. [MED]
- CLEARED: mergeOverwrite prune:false over default true (verified by render); chart pins match current versions; AppProject destinations cover every namespace; OpenBao extraEnvironmentVars SSA duplicate-key failure from 0.1.27 has NOT regressed; both probes render as exec; secret volume modes line up with runAsUser 100 / fsGroup 1000 and promtail's uid 10000.

### C42 platform-apl
- components/openbao/openbao-cert-watcher.yaml:168 — CERT-WATCHER HAS NO EGRESS under the chart's default-deny. llz-openbao-platform's openbao-default-deny (podSelector:{}, policyTypes:[Ingress,Egress]) selects every pod in llz-openbao; every allow rule in that chart selects app.kubernetes.io/name: openbao, and the watcher is openbao-cert-watcher. Zero egress — no DNS, no apiserver — so `kubectl get certificate openbao-tls` always returns empty and the loop logs "not readable yet (rbac? not issued?)" forever. At the ~80-day openbao-tls renewal NOTHING RESTARTS OPENBAO, it keeps serving the old cert, and the ESO store flips InvalidProviderConfig — precisely the cascade this Deployment exists to prevent. The file's own NP comment ("No egress restriction") assumes the pod is unisolated, which the chart makes false. [CRITICAL]
- components/llzReconciler/llz-reconciler/kustomization.yaml:8 — token-inventory.yaml IS NEVER RENDERED. `kubectl kustomize` emits 30 objects, none from that file; grep over the repo returns zero hits. The whole in-cluster writer (SA, Role/RoleBinding, 2 ExternalSecrets, NetworkPolicy, CronJob) is DEAD, while its header claims it replaces the CI step in llz-scheduled-checks.yml:285 — which still runs. Remove that CI step believing the replacement landed and llz_token_* goes absent permanently. [CRITICAL]
- components/observability/prometheus-rules/support-plane-alerts.yaml:82 — SupportPlaneDeploymentUnavailable names a Deployment that NEVER EXISTS. The regex includes platform-otel-collector, but the otel-operator derives <cr-name>-collector = otel-collector (the CR is named `otel` for exactly this reason). platform-otel-collector survives only as the legacy TLS identity. The collector half of that alert can never fire. [HIGH]
- components/observability/prometheus-rules/openbao-alerts.yaml:43 — OPENBAO ALERTS GO SILENT ON TOTAL OUTAGE. `sum(vault_core_active) < 1` and `count(vault_core_unsealed == 1) < 3` return an EMPTY vector, not 0, when the series are absent — so losing every OpenBao pod or the scrape target fires neither, and `up == 0` doesn't backstop it (the target disappears too). The sibling reconciler ruleset solves this exact class with absent(). [HIGH]
- components/llzReconciler/llz-reconciler/token-inventory.yaml:141 — DNS egress uses k8s-app: kube-dns. The documented LKE-E scar (CoreDNS is `coredns`); four other NPs in this tree carry explicit comments never to do this. Latent until the file is wired in. [MED]
- components/harbor/harbor-robot-provisioner/openbao-ca-bundle.yaml:18 — headers document optional:true mounts and an OPENBAO_SKIP_VERIFY fallback that ADR 0010 REMOVED. Same stale block in the broadPatRotator and llzReconciler copies plus broad-pat-rotator/cronjob.yaml:14. [LOW]
- CLEARED: wave-health VAP allowlist vs wavehealth.AllowedKinds/AllowedNames — no drift; llz-argo-workflows Namespace dual-ownership is not a gap (component is ManagedSkip and managedAppPlatform is validation-forced true); carved Applications use project: platform-bootstrap (destinations *).

### C43 instance-template
- .github/workflows/llz-secret-rotation.yml:514 — `rotate-state-passphrase --roots-dir terraform` points at a NONEXISTENT dir (roots are under terraform-iac-bootstrap/); all 4 roots report "skipped", reportRollover returns nil and the summary says "TF_STATE_ENCRYPTION_PASSPHRASE_OLD can now be deleted" — a false green that, if followed, PERMANENTLY DESTROYS EVERY STATE FILE. [CRITICAL]
- .github/workflows/llz-terraform.yml:166 — plan-cluster-pr has NO `environment:`, but TF_STATE_ACCESS_KEY/TF_STATE_SECRET_KEY/LINODE_API_TOKEN are EnvScope:true (envreq.go:48-50), so they resolve EMPTY and every PR plan dies at tofu init. [CRITICAL]
- .github/actions/cluster-access/action.yml:105 — forwards only state-encryption-passphrase to tf-encryption-env, DROPPING key-name / old-passphrase / old-key-name; after any passphrase rotation (which MUST introduce a new key name) every cluster-facing job fails to decrypt the kubeconfig from state. [HIGH]
- .github/workflows/llz-secret-rotation.yml:496 — `module: aws-init-only` chdirs to terraform-iac-bootstrap/aws-init-only, which exists NOWHERE; the state-passphrase job can never reach its own verb. [HIGH]
- .github/workflows/secret-rotation.yml:32 — `scope` choice list omits state-passphrase and state-passphrase-apply is never forwarded, so the rollover has NO REACHABLE ENTRY POINT and would be dry-run-only anyway. [HIGH]
- .github/workflows/promote.yml:56 — the commented `push:` trigger the file tells adopters to uncomment yields event_name=='push', which every apply job in llz-terraform.yml SKIPS: a green promotion that applies nothing. [HIGH]
- .github/workflows/llz-bootstrap-openbao.yml:863 — "Fail on bootstrap errors" tests BOOTSTRAP_ERRORS, which NOTHING EVER WRITES; a terminal gate that can never fire. [HIGH]
- COMPOUND: findings 1, 4 and 5 interact — the whole state-encryption key-rotation path is unreachable today, and the moment #4/#5 are fixed, #1 turns it into a DATA-LOSS TRAP. Fix #1 first.

### C44 .github + template-scripts
- template-scripts/ci/sbom-kubernetes.sh:30 — the `IMAGES=$(grep|awk|sed|grep -v|sort)` assignment exits non-zero under `set -euo pipefail` whenever nothing matches, so the script DIES BEFORE its "No image refs found → exit 0" guard at :36; the default source dir kubernetes/ also no longer exists in this repo. `make sbom` fails hard with no output on any machine with trivy. Reproduced. [HIGH]
- .github/workflows/llz-release.yml:170 — the `functional` job sets LLZ_FUNCTIONAL_NET=1 but NOT LLZ_FUNCTIONAL_REF, so the install/self-update gate validates the highest-semver release, not the tag just published. Promoting a back-ported patch while a newer release exists produces a green gate that never touched the new release's assets. [HIGH]
- .github/workflows/e2e-instantiate.yml:385 — the unrendered-token survivor check greps only `<@|@>`, while copier's _envops also defines `<% %>` blocks and `<# #>` comments (instance-test.sh:100 checks all six). A `<% if … %>` in any delivered file would survive the sed render, pass this check, and be FORCE-PUSHED as literal Jinja. [HIGH]
- .github/workflows/e2e-instantiate.yml:151 — the preconditions error hardcodes E2E_INSTANCE_REPO, but on the GHES lane the value comes from vars.E2E_GHES_INSTANCE_REPO; the documented GHES failure path tells the operator to set the wrong variable. [MED]
- template-scripts/ci/llz-functional.sh:125 — LLZ_FUNCTIONAL_NET=1 with unauthenticated gh records the failure and then sets SKIP_NET=0, running section B anyway and burying the real auth diagnosis under a misleading "no full vX.Y.Z release found". [MED]
- CLEARED: all 11 `llz ci` verbs the workflows invoke exist; every referenced path exists; all scripts carry the executable bit; the `[ -n "$X" ] && ARR+=(…)` idiom in build-images.yml is genuinely set-e-exempt (verified in bash); the two jq semver reducers agree with each other and with LatestLLZTag; scaffold-render-check.sh's `rm -f "$d"*.tf` touches no tracked files; the GHA ternaries in release-e2e.yml all have false fallbacks.

### C45 docs (86 files, ~23k lines)
- NOTE: `llz ci docs-guard` passes CLEAN (1050 llz invocations, 411 flags, 630 links, 38 workflow files all resolve). Every finding below is SEMANTIC — a doc that names something real but describes it wrongly.
### Delivered operator set (highest weight — deliver-docs ships these into every instance)
- playbooks/grafana-access.md:13 — whole playbook rests on secret/grafana/admin + a <release>-grafana-admin-credentials ESO Secret that the managed-only render DELETED (commit 0a169903); ADR 0005 records "grafana-admin is apl-core's own". Every credential command and the entire rotation section (:33, :82, :94) is unrunnable. Cited by operator-onboarding §6. Stale copies also at runbooks/bootstrap-openbao.md:154 and secrets.md:143. [CRITICAL]
- runbooks/orphan-volume-cleanup.md:36 (also :62,:120) — says relabeled Volumes are `<env>-…` and that --env matches `<env>-*`; the real prefix is RegionShort(env) = FIRST THREE CHARS (`pri-` for `primary`). Repeats the exact three-char-invisible bug reap.go:135 was written to fix, in a DESTRUCTIVE-SWEEP runbook. The lab/e2e examples are the one length where the bug is invisible. [CRITICAL]
- runbooks/linode-credential-rotation.md:300 — "repo scope — these are not environment-scoped" is INVERTED; envreq.go:57-58 marks both OPENBAO_SECRETS_WRITE_TOKEN and APL_VALUES_REPO_TOKEN EnvScope=true, and this same file's inventory table says to update infra-<env>. Step 4 then REVOKES THE OLD PAT while infra-<env> still holds it. [CRITICAL]
- runbooks/bootstrap-openbao.md:168 — the "required operator actions" warning is malformed: unterminated `**`, then a blank line and un-prefixed lines break out of the blockquote, so the SEAL-KEY ESCROW instruction (the only way to recover a never-printed key) and its kubectl fence render OUTSIDE the warning as three disconnected fragments with literal asterisks. [HIGH]
- runbooks/reconciler-alerts.md:51 — redirects the six OpenBao alerts to bootstrap-openbao.md, which documents NONE of them; those alerts' runbook_url annotations point there too, and the page that covers them (alerting.md) is NOT delivered to instances. A paged operator dead-ends. [HIGH]
- runbooks/first-build-failed.md:117 — "The token is destroy:<region>:<module>"; all three assert-destroy-confirm call sites pass the literal `cluster`, so the documented form fails the guard for module=all/vpc (as the snippet directly above dispatches). [HIGH]
### Spec / architecture
- secrets.md:551 — documented dual-write exit codes 2/3/4 DON'T EXIST; cli.Main maps any error to 1. Leftover from the pre-Go shell script. [MED]
- secrets.md:129 — "four least-privilege policies … no wildcard": bootstrap writes SEVEN (adds harbor-provisioner, reconciler-read, broad-pat-rotator), one with a wildcard (secret/metadata/infra/db-admin/*). Same omission at :334 for the k8s-auth roles. [MED]
- secrets.md:208 — attributes OBJ-key rotation to a linodeCredRotator CronJob that is RETIRED into a reconciler lane; the component directory no longer exists. Same claim at landing-zone-spec.md:142. [MED]
- landing-zone-spec.md:289 — objProxy missing from both the default-disabled list and "The set:" (5 default-disabled, not 4); since the doc says unknown component keys are a hard spec error, it reads as "not a valid key". [MED]
- landing-zone-spec.md:360 — platform-namespace denylist omits `obj`, which the validator DOES reject (holds the SSE-C key). [MED]
- architecture/convergence-contract.md:114 — says a hard-fail re-check "then propagates exit 1"; the code consumes the re-check verdict and only fails on TWO CONSECUTIVE hard-fails. The page's own mermaid diagram is right. [MED]
- alerting.md:143 — heading claims OTel isn't scrape-gated; it IS (defaultScrapeMonitors lists otel-collector-monitoring), per both the code and the doc's own next sentence. [MED]
- extending-llz.md:48 — arg-passthrough attributed to `drift`/`env add`, NEITHER of which forwards args (drift is cobra.NoArgs; env add is ExactArgs(1) with ~18 parsed flags). Only ext commands set DisableFlagParsing. Same false claim in the code comment at ext.go:92. [MED]
- delivery-methodology.md:44 — "seven phases", "phase 6 (handover)"; there are EIGHT (0-7), phase 6 is Sustain and 7 is Handover. [LOW]
### docs/workflows subtree
- workflows/llz-terraform.md:393 + llz-bootstrap-openbao.md:101 — OPENBAO_SECRETS_WRITE_TOKEN documented as "Actions + Secrets: write"; actual requirement is "Actions: write + Environments: write". llz-terraform.yml:503-504 records the incident: a PAT without Environments: write 403'd on the seal-key write ~5 min into a 30-min run — the repo's most common bootstrap error. [CRITICAL]
- workflows/llz-secret-rotation.md:55 (and :320) — documents the LEGACY PAT label gha-platform-platform_LINODE_API_TOKEN; rotation_identity.go:52 lists it under legacyRotationLabels ("Nothing mints or drains under them"). The live label is the spec-derived llz-<objLabelPrefix>-linode-api-token. [HIGH]
- workflows/llz-secret-rotation.md:58 — cites a nonexistent companion workflow linode-pat-revoke.yml; the drain is the `revoke-linode-pat` JOB at llz-secret-rotation.yml:411 on the caller's 30 3 * * * cron. `linode-pat-revoke` is a dispatch SCOPE, not a workflow. [MED]
- workflows/llz-terraform.md:220-225 — repo-readiness check order documented BACKWARDS; the YAML runs assert-image-fresh first under a comment stating "IMAGE SKEW IS CHECKED FIRST, AND THE ORDER IS THE WHOLE POINT". [HIGH]
- workflows/llz-scheduled-checks.md:64 — "No continue-on-error, anywhere — deliberately" is false; the Keycloak reconcile step at :133 carries it. Also five checks now, not four. [MED]
- workflows/llz-bootstrap-openbao.md:216 — claims `available` gates parallel `harbor`+`converge` JOBS that DO NOT EXIST; it is read only by ~20 in-job step ifs, and the sole cross-job gate reads ca_available. [MED]
- workflows/llz-bootstrap-openbao.md:177 — bootstrap_cluster job timeout documented 70m; YAML sets 90m (:161-162 explicitly records "Raised 70 -> 90"). The downstream arithmetic at :188 inherits the stale ceiling. [MED]
- workflows/llz-bootstrap-openbao.md:290 — says converge polls 10m; the step runs --budget 1200 (20m), which the same doc's §704-715 correctly explains as 20m. [MED]
- LOWER-RANKED VERIFIED: llz-terraform.md:435 (--volume-region vs the actual --deployment); llz-terraform.md:541-546 (destroy "Order: 1→2→3" but pre-destroy-cluster/plan-destroy-cluster have no needs: and run concurrently — YAML header repeats the error); llz-bootstrap-openbao.md:558 (wait-harbor --registry-only is deprecated and ignored); llz-secret-rotation.md:69 (scope documented as 3 values, caller offers 8; Triggers list omits the daily reaper cron); llz-secret-rotation.md:368-370 (contradicts keep-newest: "2"); llz-scheduled-checks.md:172 (credential-single-pane "two steps", now three).
- CLEARED: all of quickstart.md, local.md (durability promise traced to upgrade_policy.go snapshot/restore — it HOLDS), openbao-accounts.md, argocd-ops.md, first-workload.md, volume-labels.md, loki-access.md, reconciler-alerts.md's exprs/thresholds, workflows/README.md, llz-discover-deployments.md, llz-wedge-gameday.md, llz-breakglass-openbao.md.

### C46 shared/charty+cliopts+color+credpaths+ghaout+gitcmd+guardkit+instancelayout+pathglob+platform
- charty/charty.go:68 — SiblingValue aborts on any line indented less than the block, and COLUMN-0 YAML COMMENTS are common inside indented blocks; a comment between chart: and targetRevision: silently drops that pin from chart-pin-guard AND chart-publish-check (verified by running the package; the checked==0 backstop does not fire because other charts still yield pins). [HIGH]
- charty/charty.go:38 — trailing YAML comments are not stripped: `version: 0.1.12 # bumped` reads as "0.1.12 # bumped". Confirmed. chart-version-guard accepts a comment-only edit AS A BUMP (so an unbumped change passes green and is never published), and chart-pin-guard reports a phantom drift — the exact class the quote-stripping comment says it exists to prevent. [HIGH]
- platform/platform.go:85 — RenderTimeArtifact mixes two path spaces. docsguard's only lookup (docsguard.go:912) runs AFTER the instance-template/ prefix is stripped, so the two prefixed keys can never match; a rendered-instance doc link to terraform-iac-bootstrap/cluster/variables.tf or landingzone.yaml is reported as a broken link despite the exemption existing for it. [MED]
- color/color.go:26 — CLICOLOR_FORCE tested for NON-EMPTY rather than non-zero, so CLICOLOR_FORCE=0 (the conventional "don't force") turns color ON, injecting ANSI escapes into redirected output and step summaries. [MED]
- pathglob/pathglob.go:40 — the package-level regexp cache map is read and written WITH NO LOCK. Latent (no concurrent caller today), but the failure mode is Go's unrecoverable "concurrent map writes" fatal, reachable by adding t.Parallel() to a budget/template-manifest test or parallelizing the gate runner. [MED] (independently surfaced in C23)
- CLEAN: cliopts, credpaths, ghaout, gitcmd, guardkit, instancelayout.
- Doc rot not filed: ghaout.go:21-28 duplicated doc comment, both copies naming a function (appendGHAFile) that no longer exists, plus an orphaned baoread.RecoveryKeysFromEnv doc block at :49-53; charty.go:49-58 and :83 name/comment mismatch.

### C47 shared/portfwd+preflight+promwire+provider+s3sig+shquote+templateid+tfbin+tfvars
- s3sig/s3sig.go:56 — Region takes everything BEFORE .linodeobjects.com instead of the LAST LABEL, so a bucket-prefixed endpoint yields region `mybucket.us-ord-1` (verified by running it) → SigV4 403 that reads as "wrong key" — the exact misdiagnosis the package header exists to prevent. The generic-host branch also contradicts the doc's "falls back to us-east-1" claim. [HIGH]
- promwire/promwire.go:51 — VectorByLabel silently continues past UNDECODABLE samples, folding "could not read the answer" into "series absent"; Scalar errors on the identical body. Same shape as the empty-label skip documented at assertsecrets/rotationhealth.go:405-415 that left the presence lane permanently blind. [HIGH]
- tfvars/readregion.go:35 — the .example fallback retargets `<region>.tfvars.example`, a filename that EXISTS NOWHERE in the repo; the common "not rendered yet" failure therefore reports a path the operator has never had and never names the missing <region>.tfvars. [MED]
- configreadiness/preflight.go:137 (and :163) — broken caller of the in-scope quota arithmetic: adds=1 VPC and the full poolVCPU are charged ON TOP of a census that already includes the existing cluster, so a re-apply at the cap hard-fails claiming cluster-create will hang. Only bites when --vpc-limit/--vcpu-limit are set (see C20 — the delivered workflow leaves them 0). [MED]
- provider/boundary_test.go:36 — `forbidden` is matched with ==, so a future internal/shared/linode/<sub> package LAUNDERS THE IMPORT past the guard; the sibling test at :73 already uses HasPrefix for this reason. [MED]
- Nit: promwire.go puts its `// Package promwire …` comment AFTER the package clause, so it is invisible to go doc/pkgsite.

### C49 AGENTS.md + README + CONTRIBUTING + SECURITY + tools/AGENTS.md + .claude
- CLEAN: SECURITY.md and all 13 .claude/skills/ files — cross-links, make targets and ADR/runbook references all resolve. (release/rotate-credentials are absent from the auto-listed skills only because they correctly set disable-model-invocation: true.)
- .claude/agents/template-hygiene-reviewer.md:33 — rule 4 DEMANDS `<@ llz_version @>` placeholders in instance-template/, directly contradicting AGENTS.md:88-92 ("zero tokens"); the agent would BLESS the exact change the upgrade-churn guard fails. [HIGH]
- AGENTS.md:9 — .github/copilot-instructions.md DOES NOT EXIST, so Copilot loads no repo instructions; same false claim at CONTRIBUTING.md:80, and the line tells contributors not to create the stub. [HIGH]
- tools/AGENTS.md:27 — Layout section is stale: internal/linode/ → internal/shared/linode/; cmd/llz/wizard.go → internal/verbs/onboard/wizard.go; internal/cli/ misdescribed as rotation helpers (it is the root command tree); internal/shared|verbs|extensions/ unlisted. [MED]
- tools/AGENTS.md:53 — named seams and models DO NOT EXIST: actual are exported assertobs.WithPrometheus/WithLoki in assertobs/promquery.go + lokiquery.go, and assertobs/scrape.go + assertsecrets/openbaoaudit.go. [MED]
- README.md:136 — llz-cert-automation listed unqualified while kubernetes-charts/README.md:58 records it as NOT DEPLOYED on managed. [MED] (matches the cert-automation-is-dead-on-managed scar)
- README.md:262 — quickstart says the build takes ~20 minutes; the CLI's own guidance (cli/commands.go:144) says ~40 min. [MED]
- README.md:261 — `gh run list --limit 1` right after a dispatch can return the PREVIOUS run (or nothing), so `gh run watch` reports a stale completion. [MED]
- AGENTS.md:65 — dockerfiles layout line omits the fourth image target `llz` (README.md:154, Dockerfile:407). [LOW]
- .claude/agents/template-hygiene-reviewer.md:34 — cites the retired ../llz-node-firewall module, folded into llz-cluster/firewall.tf. [LOW]
- AGENTS.md:61 — repo layout omits top-level platform-apl/, which README.md:176 calls load-bearing. [LOW]
- AGENTS.md:13 — "Top-level directories carry their own AGENTS.md" is FALSE for terraform-modules/, kubernetes-charts/, dockerfiles/, docs/, platform-apl/. [LOW]
- .claude/hooks/format-on-edit.sh:14 — `*/tools/*.go` and `*/terraform-modules/*.tf` require a LEADING PATH SEGMENT, so a repo-relative file_path silently skips formatting and the hook still exits 0. [MED]

### C48 Makefile + Dockerfile + copier.yml + lint configs
- dockerfiles/Dockerfile:47 — `-X main.version=${LLZ_VERSION}` names a symbol that no longer exists (the var is internal/cli.Version); the linker drops it silently, so every published image bakes `llz version == "dev"`, which makes `llz ci assert-image-fresh` warn-and-pass FOREVER in every instance's CI. [CRITICAL — independently found in C39]
- Makefile:1282 — sbom-terraform scans terraform-iac-bootstrap/, which does not exist at the repo root; sbom-kubernetes scans a non-existent kubernetes/. `make sbom` fails once syft/trivy are installed, and no workflow runs it (the release.yml these comments describe is gone). [HIGH]
- Makefile:454 — tf-validate hardcodes `terraform`, the one binary the Dockerfile deliberately refuses to provide; exits 127 in ci-tofu and the devcontainer. No caller, so it never fails visibly. [MED]
- Makefile:1171 — `make lint` EXITS 0 HAVING RUN NOTHING (including the whole gate suite) when the only working-tree changes are untracked new files; verified empirically. [CRITICAL]
- .checkov.yaml:28 — CKV_TF_2 is "module sources use a tag with a version number" (verified via `checkov --list`), NOT the local_sensitive_file check the comment claims; the module-pin assertion is globally disabled under a FALSE JUSTIFICATION. CKV_K8S_30 above it can never fire under --framework terraform. [HIGH]
- dockerfiles/Dockerfile:146 — the toolbox stage CLAIMS to checksum-verify every CLI; kube-linter, kubeconform, argo, yq, uv and actionlint are UNVERIFIED, and actionlint is `curl | bash` of an UNPINNED main-branch script. [HIGH]
- Makefile:364 — install-tools omits tofu, tflint, kube-linter, kubeconform, kustomize, promtool and gitleaks, so the documented onboarding sequence dies at the first lint step. [MED]
- Makefile:436 — `TF_DIRS := $(wildcard …)` silently drops a renamed module, turning all four TF gates into no-ops for it. [MED]
- .trivyignore:1 — three Critical-CVE suppressions for `make sbom-scan`, a gate with no caller; trivy AUTO-LOADS the file, so they'd silently apply to any future trivy step. [MED]
- .kube-linter.yaml:56 — probe/restart-policy checks disabled REPO-WIDE on a CronJob-only rationale; the first Deployment added ships probe-less and green. [MED]
- Makefile:1424 — coverage-bank reads coverage/tools.out with no prerequisite producing it. [LOW]
- VERIFIED CLEAN: both budget files current (untestable-loc 524/526, core-surface 1948/1948 and 6/6 exactly); copier.yml validators, _exclude/_skip_if_exists and --trust handling correct; coverage floors list intact.

## Round 2 (5 highest-severity chunks re-reviewed with round-1 findings excluded)

### R2-credrotate + statepassphrase (5 new)
- credrotate/broadpat.go:260 — RevokeOldBroadPATs measures the grace window from the sibling's Linode `created` time, NOT from when it was superseded. At ROTATE_AFTER_DAYS=60/GRACE_DAYS=7 the previously-live broad PAT is always ~60d old, so it is DELETED SECONDS AFTER step 3 publishes its replacement. Any in-flight llz-terraform.yml apply (which resolved LINODE_API_TOKEN at job start) starts 401ing mid-apply. The grace window only ever protects orphans from a failed run — never the live token, which is exactly what the comment claims it protects. [CRITICAL]
- credrotate/inclusterpat.go:197 — same shape, monthly cadence. RunPATRevokeOld(…, inclusterPATGraceDays=7) runs seconds after secretPropagatorKVPut, so the ~30-day-old LIVE PAT is revoked while ESO (5m refresh) + kubelet (~1m) still serve the old value. Up to ~6 minutes where the volume-labeler, cidr-firewall, DNS-01 solver webhook and ExternalDNS all hold a revoked token. Contrast table.go's IDsToDrain and objkey.go's revoke-old, which get real overlap via keep-newest-2. [CRITICAL]
- verbs/onboard/wizard.go:361 — PushSecrets wraps DropStatePassphraseIfLive in `if rerr == nil` on ResolveInstanceRepo; when the repo can't be resolved the clobber guard is SKIPPED SILENTLY and a stale cached TF_STATE_ENCRYPTION_PASSPHRASE is pushed over the live one. The guard's own doc calls itself "the LAST word before any gh secret set". [HIGH]
- statepassphrase/deps.go:37 — the uninstalled Deps defaults still FAIL OPEN: Exec returning (nil,nil) makes ghSecretPresent answer present=true, answered=true for every path. extensions_init.go already names this package "THE DANGEROUS ONE" and states the doctrine that the default should error; the wiring was added but the default was not fixed, so the trap is armed for the next entry point. [HIGH]
- credrotate/tempobjkey.go:49 — hand-rolled endpoint→cluster parsing (`contains "/"` is the only check) alongside the strict objClusterFromEndpoint in the same package. A virtual-host-style endpoint yields "<bucket>.us-ord-1" and mints against a nonexistent cluster instead of refusing. [MED]

### R2-capability + clusterspec (4 new)
- capability/writer.go:194 — deleteFlagPrefixes omits `--field-selector=`, so Writer.Delete REFUSES assertplatform's workflow reap on every call; the caller discards the error, so the reap silently never runs. CONFIRMED against assertplatform/healthworkflow.go:160. [HIGH]
- capability/capability.go:514 — execStdin uses CombinedOutput() while the argv seam (kubectlprobe.Exec) uses .Output(); CreateStdin therefore returns STDERR MERGED INTO THE JSON its only caller strictly json.Unmarshals, turning a successful Workflow submission into "could not read its name". The stub in WithExec hides it from tests. CONFIRMED. [HIGH]
- capability/writer.go:261 — Annotate's keyValue is the only own-element parameter that skips safeArg, so `--all=true` reaches kubectl as a flag, breaking the header's stated invariant. [PLAUSIBLE/MED]
- clusterspec/derived_values.go:78 — the comment justifies envPairRe's brittleness by a caller-side "found nothing" guard; render.go:484 (the sole caller) HAS NONE, so a renderer that stops matching the `value: "..."` shape silently validates nothing. [PLAUSIBLE/MED]

### R2-platform-apl (7 new)
- components/observability/otel-bootstrap-ca.yaml:125 — the two otel-collector.llz-observability.svc* SANs added here for the now-mTLS OTLP receiver are STRIPPED on every rendered instance: RenderOtelSANPatch (clusterspec/kustomize.go:796) replaces spec.dnsNames WHOLESALE with only otel.<env>.internal + the two platform-otel-collector.* names. Any client dialling otel-collector.llz-observability.svc:4318 and verifying the chain gets a hostname mismatch — the exact failure the comment at :116-120 claims to have fixed. [HIGH]
- components/observability/prometheus-rules/support-plane-alerts.yaml:176 — OTelCollectorRefusingData joins three sum(rate(...)) terms with `+`; a vector-+-vector with ONE EMPTY OPERAND yields empty, so the alert can never fire unless all three per-signal counters exist. The inline comment asserts the opposite. [HIGH]
- support-plane-alerts.yaml:192 — OTelCollectorExportFailures has the identical defect. [HIGH]
- support-plane-alerts.yaml:249 — HarborJobQueueBacklog uses bare max(...), discarding the `type` label its summary/description interpolate; fires as "Harbor job queue is backing up ()". Same mistake the reconciler's `by (cred, class)` comment exists to prevent. [MED]
- components/openbao/openbao-cert-watcher.yaml:134 — the rotation baseline is PROCESS-LOCAL. Any watcher restart (node drain, image bump, Argo sync — maxSurge:0 means a real gap) re-baselines on the already-rotated value, so a rotation during downtime is silently missed and OpenBao serves the stale cert forever → the ESO InvalidProviderConfig cascade this Deployment exists to prevent. [HIGH]
- components/broadPatRotator/broad-pat-rotator/openbao-ca-bundle.yaml:18 — header claims the CronJob mount is optional:true with an OPENBAO_SKIP_VERIFY fallback; the mount is NON-optional (cronjob.yaml:121) and the flag is gone. Same dead text in llzReconciler/openbao-ca-bundle.yaml:19 and harbor-robot-provisioner/cronjob.yaml:27. [LOW]
- components/harbor/harbor-robot-provisioner/network-policy.yaml:26 — DNS comment says sidecar injection was disabled; cronjob.yaml:82 sets inject:"true", and the istiod egress rule 30 lines below exists only because the pod is meshed. [LOW]

### R2-instance-template (5 new)
- VERIFIED: .template-managed.lock digests all 30 match the tree — no drift.
- .github/workflows/llz-secret-rotation.yml:665 — rotate-db-admin opens the LKE-E control-plane ACL via cluster-access but has NO PAIRED `lke-runner-acl mode: revoke` (and no kubeconfig cleanup). Every other cluster-access caller in the repo has one, and the action's own header says the caller MUST. Each db-admin rotation PERMANENTLY LEAVES AN EPHEMERAL HOSTED-RUNNER IP ALLOWED on the API server. [CRITICAL]
- .github/workflows/llz-terraform.yml:745 — bootstrap-openbao needs apply-cluster + apply-object-storage but NOT apply-databases. db-declared reads the tfvars while seed-db-admin/rotate-db-admin read Terraform STATE, so on module:all (or any module:cluster rebuild) the seed silently no-ops, secret/infra/db-admin/<name> is never written, and the provisioning password rotate-on-create exists to kill STAYS LIVE IN STATE — build green. [CRITICAL]
- .github/workflows/secret-rotation.yml:118 — no state-passphrase-apply dispatch input and none forwarded in with:, so the state-encryption rollover is PERMANENTLY REPORT-ONLY. Distinct from the already-reported missing scope option: fixing that alone still leaves the job unarmable. [HIGH]
- environments/prod-web-ord.yaml.example:82 — documents `keyRotationDays: 90 # → obj_key_rotation_days` as live, but the field is DEPRECATED/ignored and the TF variable was removed. The delivered example is the only starting reference an adopter copies. [MED]
- .github/workflows/llz-cluster-health.yml:38 (also llz-wedge-gameday.yml:40, llz-scheduled-checks.yml:27) — env-scoped secrets declared required:true in workflow_call, contradicting the incident-derived "never do this" comment in four sibling reusables. PLAUSIBLE: lines date to the initial release and the scheduled checks demonstrably run, so secrets:inherit apparently skips the check today — but four files assert the opposite and nothing gates it either way. [PLAUSIBLE/MED]

### R2-converge (7 new)
- health.go:706 — checkStorageClasses feeds sectionItems' nil-on-failure list into DefaultStorageClasses, so an unreadable `get storageclass` records CatFail "no default StorageClass" (a HARD STRIKE) on top of the CatPending it already recorded; two consecutive blips abort converge with "operator intervention required". [HIGH]
- health.go:1070 — the 256KB annotation-wedge self-heal keys on a.OpErr, which ParseArgoApp fills only for phase Failed/Error; a sync retrying IN PLACE keeps phase Running and puts the "annotations: Too long" text in SyncErr, so StripOversizedCRDLastApplied never runs and converge polls the wedge to budget exhaustion. [HIGH]
- health.go:39 — healthNamespaces OMITS llz-reconciler, llz-argo-workflows, llz-argo-events, llz-pat-rotator, obj-proxy and monitoring, all created by platform-apl/components/* / llz-cluster-foundation; workload, Service and default-deny-NetworkPolicy checks silently skip them (a Deployment whose pods are rejected at admission creates no Pod, so `get pods -A` misses it too). [HIGH]
- nudge.go:95 — the "block until the ClusterSecretStore is Ready" step uses a bare `kubectl wait --for=condition=Ready`, which errors INSTANTLY on a not-yet-created store/CRD (the exact post-seed race it exists for) while logging "not Ready within 300s"; wait.go in the same package already documents and works around this with --for=create. [HIGH]
- health.go:745 — the Loki-S3 section is gated on Exists("get secret obj-secrets …"), which reads an unanswerable probe as absent and returns BEFORE EVEN PRINTING A HEADER, silently removing the obj convergence gate from that poll. Compare checkFirewallBootstrap's correct ExistsOK handling of the same shape. [HIGH]
- health.go:937 — the OpenBao audit-device probe uses Exists on a `kubectl exec … test -s`, so an exec that COULD NOT RUN hard-fails as "audit device inactive"; the message carries no tunnel signature so record() cannot downgrade it, and the local tunnelBlocked flag is not consulted. [MED]
- health.go:814 — the firewall ConfigMap is re-probed with plain Exists, throwing away the cmExists/cmAnswered pair obtained 35 lines earlier; a blip on the second call hard-fails a ConfigMap the same scan already saw. [MED]
- NOT FILED: cli/ci_converge.go:43 installs the `drive` binding's Writer into the package-global deps, so all three bindings — including the read-only health and health-incluster ASSERTIONS — share a write-capable handle, contradicting extension.go's claim that "drive holds cluster-write, health does not, and the two cannot be confused".
