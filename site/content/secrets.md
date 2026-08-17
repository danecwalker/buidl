---
title: Secrets
group: Guides
description: Names in buidl.yaml. Values in files or the process environment.
---

`env.secret` lists names only. Values are resolved at deploy time, lowest precedence first:

| Source | Committed | Notes |
|---|---|---|
| `.env`, `.env.<environment>` | usually | only when `env.dotenv: true` |
| `.buidl/secrets-common` | yes | indirections only: `DATABASE_URL=$PROD_DATABASE_URL` |
| `.buidl/secrets` | no | all environments |
| `.buidl/secrets.<environment>` | no | one environment |
| process environment | n/a | always wins |

The process environment outranks every file, so CI injection is never overridden by a leftover file on a laptop. A `$VAR` value looks up the environment. An unresolved indirection is treated as missing, not deployed as the literal string `$VAR`.

Values go into a Kubernetes Secret, never into `buidl.yaml`. A checksum of the resolved secrets is annotated on the pod template, so changing a secret value rolls the pods.

```
buidl variable list
buidl variable set DATABASE_URL=postgres://…
buidl variable set LOG_LEVEL=info --clear
```

`variable` (alias `var` / `vars`) prints each variable, its kind, and where it came from. Secrets show as `set, N chars`, never the value. Accessory secrets such as `POSTGRES_PASSWORD` are resolved from the same files even when they are not listed under the app's `env.secret`; they are not injected into the app.

For secrets already in the cluster (External Secrets, Vault), use `env.secretRefs`.

`env.dotenv: true` reads `.env` and `.env.<environment>` for declared names only. A stray local variable cannot become part of a release. `.env.local` and `.env.<environment>.local` are never read.

`buidl init` adds `.buidl/secrets` and `.buidl/secrets.*` to `.gitignore`. It does not ignore `.buidl/`, because `secrets-common` and hooks are meant to be committed.

```
.buidl/
  secrets-common          committed, indirections only
  secrets                 gitignored
  secrets.production      gitignored
  hooks/
    pre-deploy            executable
    pre-deploy.sample     inert until you chmod +x
```
