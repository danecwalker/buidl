---
title: No registry
group: Guides
description: Build locally, copy the archive onto each server, delete the tars.
---

`buidl init` without `--registry` writes `image: buidl.local/<app>`.

```sh
buidl init
buidl add server 203.0.113.10 --email you@example.com
buidl add domain example.com
buidl deploy
```

`deploy` then:

1. Builds a local archive with BuildKit.
2. Copies it onto every `infra.servers` node.
3. Imports it into containerd (`k3s ctr` / `rke2 ctr` into the `k8s.io` namespace).
4. Deploys with `imagePullPolicy: Never`.
5. Deletes the temporary tars on both sides.

Nothing is stored in a local Docker image store. You do not need a registry account, a pull secret, or `docker login`.

## What you do need

SSH to the nodes. A kubeconfig-only cluster without `infra.servers` cannot take this path — there is nowhere to copy the archive.

The image name `buidl.local/*` (and the older placeholder `ghcr.io/change-me/*`) is what marks a local image. Those still sideload.

## With a registry

```sh
buidl init --registry ghcr.io/myorg
```

BuildKit exports straight to the registry. The cluster pulls by digest. `registry.createPullSecret` defaults to true, so the same credential BuildKit pushed with is copied into the cluster as an imagePullSecret. GHCR packages are private until you flip them; without the pull secret the first deploy builds, pushes, then dies on `ErrImagePull`.

```yaml
registry:
  createPullSecret: true
```

Set `createPullSecret: false` for a public image or when the nodes already have a `registries.yaml`.
