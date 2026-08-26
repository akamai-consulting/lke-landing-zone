# An apply wants to replace your Object Storage buckets

**Symptom.** The `object-storage` lane refuses the apply:

```
✗ this upgrade proposes destroying or replacing 2 live resource(s):
    module.object_storage.linode_object_storage_bucket.loki_chunks — replace (delete,create)
    module.object_storage.linode_object_storage_bucket.harbor_registry — replace (delete,create)

THESE ARE BUCKET RENAMES, WHICH ARE NOT RECOVERABLE BY RETRYING.
```

**This is the gate working.** A bucket label is create-time only, so the only way
Terraform can honour a changed label is to **delete the bucket and create an empty
one** — and Loki's chunks and the Harbor registry live in those buckets. Linode
refuses to delete a non-empty bucket, which is the only reason a mistake here has
so far been a failed run rather than a silent one.

## Why it happens

`spec.instance.objLabelPrefix` prefixes every bucket and key label this instance
owns. It defaults to the **instance name**. Instances created before that field
existed got the module's hardcoded `platform`, so on those the default is a
rename of every bucket they have.

Nothing about the upgrade diff shows this: the Terraform roots are generated and
gitignored, and no pull request runs a plan. It surfaces at the apply.

## Find out before you merge

You do not have to reach the apply to learn this. Run the same check locally, from
your instance checkout:

```bash
cd terraform-iac-bootstrap/object-storage
llz tofu --region <env> -- init -upgrade
llz tofu -- plan -var-file=<env>.tfvars -out=tfplan.bin
llz tofu -- show -json tfplan.bin | llz ci assert-upgrade-plan
```

It exits 0 if nothing is at risk. `llz upgrade` prints these when your instance has
not pinned the prefix.

> Every OpenTofu line goes through `llz tofu`, **including `show`** — the plan file
> is encrypted too, so reading it needs the same `$TF_ENCRYPTION` that writing it
> did. In the pipeline that variable is exported for the whole job, which is why
> the workflow's own copy of this line uses a bare `tofu`.

## Fix it

**1. Ask what each bucket actually holds.** The refusal already tells you — it
reads the live object counts from the Object Storage API and prints them:

```
WHAT EACH BUCKET ACTUALLY HOLDS:

    platform-harbor-registry-prod                46 objects
    platform-loki-chunks-prod                    63345 objects
    gsap-apl-loki-admin-prod                     EMPTY
    gsap-apl-loki-ruler-prod                     EMPTY
```

**2. Follow the recommendation, if there is one.** When one prefix moves only
empty buckets, that is the answer and the refusal names it:

```yaml
spec:
  instance:
    objLabelPrefix: platform
```

Then `llz render`, commit, and re-run the apply. The buckets holding data stay
exactly where they are; the empty ones are replaced, and the gate passes them for
that reason — it verifies emptiness against the API rather than inferring it from
the plan.

The refusal checks the **key** labels too, and says whether they already agree
with the prefix it is recommending — the same prefix names both, and `llz reap`
plus the rotation table match key labels exactly, so a value that is right for the
buckets and wrong for the keys moves the problem into rotation.

**3. When both directions would destroy data**, there is no prefix to pin and the
check will not guess. Decide per bucket. For each one you do intend to rename:

- **Copy the objects across** to the new name.
- **Repoint the consumer** — Loki and Harbor read the bucket name from the
  rendered values, so `llz render` moves them together with the spec.
- **Remove the old bucket down the destroy path**, which is where the
  confirmation for deleting a bucket lives. Do not reach for the apply lane.

## Avoiding it next time

`llz upgrade` warns when an instance has not pinned the field, because that is the
state where the default can silently become a rename. Pinning it — to whatever
your buckets already use — makes the value explicit and the warning stop.

## What the gate will and will not allow

| Plan proposes | Verdict |
|---|---|
| Replacing a bucket the API reports as **empty** | **Allowed**, reported loudly with the count |
| Replacing a bucket holding **any** objects | Refused |
| Destroying a bucket outright (not replacing it) | Refused, whatever it holds |
| Replacing anything that is not a bucket | Refused |
| Any of the above when the object count **could not be read** | Refused — the exemption is granted on evidence, and its absence is not evidence |

## Related

- [first-build-failed](first-build-failed.md) — the `[400] … already exists`
  row, which is the mirror image of this one: a bucket label colliding with
  another instance's rather than moving away from your own.
- [linode-credential-rotation](linode-credential-rotation.md) — the OBJ **key**
  labels the same prefix names.
