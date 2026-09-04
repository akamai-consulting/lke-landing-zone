# Instance-local documentation

**This file is yours.** It is `owned` in `.template-manifest` — seeded once when
the instance is scaffolded and never touched again by `llz upgrade`. Everything
else under `docs/`, plus `README.md` and `AGENTS.md`, is `managed`: the template
overwrites it on every upgrade.

That is why this file exists. An instance that extends the platform — its own
charts, its own workflows, its own runbooks — needs somewhere to link them from
that an upgrade will not silently remove. Link them here.

## Why not just edit the other docs

You can, and the edit will survive until your next `llz upgrade`, at which point
the overwrite pass replaces the file from a clean render of the new release and
your links are gone. The subsystem they pointed at is untouched — it is only the
references that vanish, which is the harder failure to notice, because nothing is
broken and nothing reports it.

This happened to a real adopter: local managed-apps references were dropped from
six `managed` files in one upgrade, leaving the playbook they pointed at present
but unreachable. `llz upgrade` now reports references it drops to paths that
still exist in your instance, so you will hear about it — but the durable place
to put them is here.

## What to put here

- Charts, overlays and workloads this instance adds on top of the platform
- Runbooks for systems the template does not know about
- Team conventions, on-call notes, escalation paths
- Anything you would otherwise have appended to `README.md` or `docs/README.md`

Subdirectories you create under `docs/` are yours too. The template only
overwrites the files it actually ships, and `llz ci deliver-docs` prunes only what
it just delivered — so a directory of your own alongside the delivered `runbooks/`
and `playbooks/` persists across upgrades. What it cannot do is keep a link to it
alive from a file the template owns, which is what this page is for.

> **One name to avoid.** The prune removes by top-level *name*, so a directory of
> yours that reuses a name the template ships (`designs/`, `architecture/`,
> `adr/`, `infosec/`, `workflows/`) is indistinguishable from the delivered one and
> is pruned with it. Pick a name of your own — your org, your team, your instance.
>
> This paragraph used to promise that the template "never deletes one it does not
> know about". It did: until the fix that added this note, the prune walked your
> `docs/` and removed every top-level entry outside `{quickstart.md, runbooks,
> playbooks, README.md, local.md}` — your directories included, on every render and
> every `copier update`. If you are on an older `llz`, `llz self-update` before your
> next `llz upgrade`.

<!-- Add your links below. -->
