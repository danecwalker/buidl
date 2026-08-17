---
title: Commands
group: Operate
description: The front door, then the hidden commands that still work.
---

Everything you run in a directory is an app. Default help is this list.

| Command | Purpose |
|---|---|
| `init` | detect the project, write `buidl.yaml` and Dockerfile; ask about CI / staging / review apps |
| `add server` | list a machine you already have |
| `add domain` | hostname on an app (`--app` to pick; default = first) |
| `add postgres` / `add redis` | stateful app |
| `add NAME` | extra process app (`--image`, `--host`, `--port`, `--command`) |
| `deploy [APP]` | converge the cluster if needed, then build, push, apply, wait |
| `status [APP]` | live release, health, instances |
| `watch [APP]` | live dashboard: health, RAM, CPU, uptime |
| `logs [APP]` | stream logs (`-F` to follow) |
| `rollback [APP]` | previous release, or `--to <id>` |
| `destroy` | tear down the app (`-e` required when overlays exist) |
| `update` | install the latest buidl release |

`deploy --dry-run` prints the plan. `status --history` lists releases. `deploy postgres` reconciles that stateful app.

## Global flags

| Flag | Purpose |
|---|---|
| `-e` / `--env` | environment to target |
| `-f` / `--config` | path to `buidl.yaml` |
| `-o` / `--output` | `auto`, `pretty`, `plain`, `json` |
| `-v` / `--verbose` | detailed progress |
| `--no-color` | disable color |
| `--timeout` | abort after this duration (default 30m; `watch` ignores it unless you set it) |

## init

```
buidl init
buidl init --registry ghcr.io/myorg
buidl init --no-ci --staging --preview
```

| Flag | Purpose |
|---|---|
| `--app` | application name (default: detected) |
| `--image` | image repository |
| `--registry` | registry host used to build the image reference |
| `--force` | overwrite existing files |
| `--ci` / `--no-ci` | GitHub Actions, skip the question |
| `--staging` | staging overlay and a promote-to-production workflow |
| `--preview` | review app per pull request |
| `--no-dockerfile` | skip generating a Dockerfile |

## add

```
buidl add server 203.0.113.10 --email you@example.com
buidl add server 203.0.113.11 --role worker
buidl add domain example.com
buidl add domain api.example.com --app api
buidl add postgres
buidl add redis --disk 20Gi
buidl add api --image ghcr.io/acme/api --host api.example.com
buidl add worker --command ./worker
```

`add server` never creates the VM. `--email` is the Let's Encrypt contact and is required once a domain (TLS) is configured. The first server becomes the control plane.

A second domain without `--app` is an extra hostname on the first app. A separate process is `buidl add api`.

## deploy

```
buidl deploy
buidl deploy --dry-run
buidl deploy --dry-run --detailed-exitcode
buidl deploy postgres
buidl deploy --skip-cluster
buidl deploy --skip-build --digest sha256:...
```

| Flag | Purpose |
|---|---|
| `--dry-run` | print the plan and change nothing |
| `--detailed` | full object diffs (with `--dry-run`) |
| `--detailed-exitcode` | exit 2 when `--dry-run` detects changes |
| `--auto-rollback` | revert if the rollout fails |
| `--skip-cluster` | do not inspect or change `infra` |
| `--skip-build` | deploy an image already in the registry |
| `--digest` | pin this exact digest |
| `--allow-dirty` | allow uncommitted changes |
| `--yes` | skip confirmation prompts |
| `--no-wait` | return without waiting for healthy |
| `--no-cache` | ignore the build cache |

Plan output names each object, the fields that change, and the runtime effect (`replaces N instances`, `scales to N`, `no restart`, `switches live traffic`, `publishes externally`). Secret changes are reported by key name only. Values never appear.

`-o json` emits newline-delimited events. Release ID, digest, and URL are also exported as CI step outputs.

## status, logs, rollback, watch

```
buidl status
buidl status --history
buidl logs -F
buidl logs --since 10m
buidl rollback
buidl rollback --to <id>
buidl watch
buidl watch --once
```

See [Watch]({{< relref "watch" >}}).

## destroy

```
buidl destroy
buidl destroy -e preview
buidl destroy --stale 7d
buidl destroy --dry-run
```

On a single-target file (no overlays), `destroy` does not need `-e`. When overlays exist, `-e` is required — omitting it does not silently target staging. Production also requires `--force`. A preview environment is disposable: `destroy -e preview` deletes its namespace. Long-lived environments keep their namespace and any accessories; only the app objects are removed.

`--stale 7d` deletes preview namespaces older than the duration.

## update

```
buidl update
buidl update --check
```

## Hidden commands

Also implemented, hidden from default help:

`environment`, `variable`, `cluster`, `promote`, `build`, `manifest`, `config`, `hooks`, `plan`, `releases`, `accessory`, `add app`.

`plan` is `deploy --dry-run`. `releases` is `status --history`. `environment` edits overlays; it does not create or destroy cluster objects. `cluster` is for inventory, status, kubeconfig, and reset.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | failure |
| 2 | changes detected (`deploy --dry-run --detailed-exitcode`) |
| 3 | invalid configuration |
