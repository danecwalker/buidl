---
title: Stack
group: Guides
description: One directory is one stack. Everything you run in that stack is an app.
---

The first process is the top-level `app` / `image` / `deploy` / `proxy` in `buidl.yaml`. Extra processes live under `apps:`. Postgres and Redis stay under `accessories:` because they are typed and stateful.

```sh
buidl add domain example.com
buidl add postgres
buidl add redis
buidl add api --image ghcr.io/myorg/api --host api.example.com
buidl add worker --command ./worker
buidl deploy
```

A stack `deploy` rolls out every process app and creates any missing accessory. Later deploys leave existing accessories alone — including ones that have drifted — so shipping a web change cannot restart a database.

## Domains

A second `add domain` is an alias on the first app: one Ingress, one certificate, every name on the same Service.

```sh
buidl add domain example.com
buidl add domain www.example.com
```

A separate process does not inherit the first app's host. Give it one:

```sh
buidl add api --host api.example.com
# or later:
buidl add domain api.example.com --app api
```

## Stateful apps

```sh
buidl add postgres
buidl add redis --disk 20Gi
```

That writes `type: postgres` (or `redis`) under `accessories` and generates `POSTGRES_PASSWORD` plus `DATABASE_URL` into `.buidl/secrets`. Image, port, volume and mount path are filled at load.

```yaml
accessories:
  postgres:
    type: postgres
    # image, port, storage, and POSTGRES_PASSWORD are defaulted
```

An explicit `image` / `storage` still wins. Untyped accessories with a full spec keep working.

Each one becomes a StatefulSet plus a headless Service. Inside the namespace, `postgres` resolves as `<app>-postgres`. Typed (or well-known) Postgres and Redis images get exec probes so a first boot is covered by startup rather than a hair-trigger liveness restart.

Images are not digest-pinned (buidl did not build them). They are never deleted: removing one from `buidl.yaml` stops managing it. You delete the StatefulSet and volume yourself.

Reconcile one with `buidl deploy postgres`. It prompts before anything that restarts a pod.

They have unit tests and render through the real Kubernetes scheme. They have not been applied to a real cluster yet. Treat the first one as an experiment.

## Targeting one app

```
buidl deploy api
buidl status worker
buidl logs api -F
buidl watch api
buidl rollback api
```

`destroy` tears down process apps and leaves accessories in place.
