---
title: Environments
group: Guides
description: Overlays on the base file. Production is never implied from an empty -e.
---

Most apps never need an `environments` block. One live target is enough. Add an overlay when you want a second target (GitHub Actions staging, a preview per PR).

`init` asks on a terminal. Scripts pass `--staging` and `--preview`.

```sh
buidl environment list
buidl environment new staging --host staging.example.com
buidl environment new production --host example.com
buidl environment set staging
```

The first overlay you create becomes `defaultEnvironment`. Production is never implied from an empty `-e` when several overlays exist and no default is set.

Environments deep-merge onto the base. Maps merge key by key. Sequences are replaced, so `platforms: [linux/arm64]` in an overlay means exactly that list.

`environment` (alias `env`) edits the overlays. It does not create or destroy cluster objects.

## What init can write

```yaml
environments:
  staging:
    proxy: {host: staging.acme.com}

  production:
    deploy:
      autoscale: {min: 5, max: 20}

  preview:
    deploy:
      kubernetes:
        namespace: web-preview-${BUIDL_SLUG}
        createNamespace: true
        ephemeral: true
    proxy:
      host: ${BUIDL_SLUG}.preview.acme.com
```

A preview environment is disposable. `buidl destroy -e preview` deletes its namespace. Long-lived environments keep their namespace and any accessories; only the app objects are removed, and production also requires `--force`.

`destroy --stale 7d` is the backstop for a missed close event: it deletes preview namespaces older than the duration. On a single-target file (no overlays), `buidl destroy` does not need `-e`.

## Built-in variables

`${VAR}`, `${VAR:-default}`, and `${VAR:?why it is needed}` are expanded after the YAML is parsed, so a value with YAML metacharacters cannot break the file structure.

buidl sets:

| Variable | Value |
|---|---|
| `BUIDL_ENV` | selected environment |
| `BUIDL_SHA` / `BUIDL_SHORT_SHA` | commit sha |
| `BUIDL_BRANCH` | branch (recovered from CI on detached HEAD) |
| `BUIDL_SLUG` | DNS-safe branch slug, or `pr-<n>` in CI |
| `BUIDL_GIT_TAG` | exact-match git tag, if any |
| `BUIDL_PR` | pull request number in CI |
| `BUIDL_CI` | CI provider, when detected |
| `BUIDL_VERSION` | buidl version |

`BUIDL_SLUG` is how preview environments get a hostname and namespace without extra config.

## Promote

Hidden, still implemented: `buidl promote --from staging --to production` ships the digest already running in staging. It does not rebuild.
