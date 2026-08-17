---
title: Config
group: Reference
description: Commands write this file. Open it when you need an override.
---

A minimal file is enough to deploy:

```yaml
app: web
image: ghcr.io/acme/web
```

That gives you a HorizontalPodAutoscaler (CPU 70%, bounds from the fleet or a 1–4 fallback), port 8080, `/livez` `/readyz` `/startupz` probes, a blue-green update, a non-root pod with all capabilities dropped, a namespace named after the app (created on first deploy), and an imagePullSecret copied from your local Docker login so the cluster can pull the image you just pushed. Set `replicas` to pin a static count. Set `deploy.strategy.type: rolling` to keep a rolling update. Set `createNamespace: false` if you manage the namespace yourself. Preview environments stay at one replica.

Schema stays `version: 1`.

## A more complete file

```yaml
version: 1
app: web
image: ghcr.io/acme/web

build:
  driver: buildkit          # or prebuilt, to deploy an image that already exists
  dockerfile: Dockerfile
  platforms: [linux/amd64]
  cache: registry           # survives ephemeral CI runners
  secrets:
    npm_token: env:NPM_TOKEN   # BuildKit mount, never written into a layer

deploy:
  target: kubernetes
  port: 3000
  healthcheck:
    # defaults: /readyz, /livez, /startupz
    # path: /up   # one endpoint for all three (Rails/Kamal)
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {memory: 512Mi}
  strategy:
    type: bluegreen         # bluegreen | rolling | recreate
  drainTimeout: 30s
  deployTimeout: 5m

env:
  clear:
    LOG_LEVEL: info
  secret: [DATABASE_URL]

proxy:
  host: acme.com
  hosts: [api.acme.com]     # extra names on the same app
  ssl: true                 # cert-manager issues the certificate
```

Top-level `image` / `deploy` / `proxy` is the first process. Extra processes go under `apps:` and do not inherit the first app's host. Typed Postgres and Redis stay under `accessories:`.

## Private registries

```yaml
registry:
  createPullSecret: true          # the default
  # pullSecret: my-registry-creds # or a secret you already manage
```

Push credentials come from the standard Docker config (`docker login`, `gcloud auth configure-docker`, `docker/login-action`).

## What gets applied

Writes use server-side apply with field manager `buidl`. Per app and environment that is a `ServiceAccount`, `Deployment`, `Service`, and optionally `Secret`, `Ingress`, `HorizontalPodAutoscaler`, `PodDisruptionBudget`, `Namespace`. Objects get `app.kubernetes.io/*` labels and `buidl.dev/*` annotations (release, digest, commit, actor, timestamp).

Hidden: `buidl config show`, `buidl config validate`, `buidl manifest`.
