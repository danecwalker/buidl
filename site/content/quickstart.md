---
title: Quick start
group: Start
description: Detect the project, list a machine you already have, deploy a digest.
---

Needs a kubeconfig or a server you can SSH to, and a BuildKit endpoint. See [Install]({{< relref "install" >}}).

```sh
cd my-app
buidl init                         # or: buidl init --registry ghcr.io/myorg
buidl add server 203.0.113.10 --email you@example.com
buidl add domain example.com
buidl add domain api.example.com   # same app, extra hostname
buidl add postgres                 # optional
buidl add api --image ghcr.io/myorg/api --host api.example.com
buidl deploy
```

That is the happy path. You should not need to edit `buidl.yaml` for it — the commands write the file, and omitted settings have safe defaults.

`deploy` can also install k3s or RKE2 on machines you already have. Creating those machines is not buidl's job. Use OpenTofu, Terraform, Ansible, or a cloud console, then `buidl add server`.

## What init writes

`init` detects Go, Node, Python, Ruby, Rust, and static sites. If there is no Dockerfile it writes a multi-stage one. It also scaffolds `.buidl/` (secrets + hooks).

On a terminal it asks whether you want GitHub Actions, then staging, then review apps. Scripts pass `--ci`, `--staging`, `--preview`, or `--no-ci` instead.

Omit `--registry` to build locally and copy the image onto your servers. Pass `--registry ghcr.io/myorg` to push instead. See [No registry]({{< relref "no-registry" >}}).

## One live app

One live app, blue-green updates, no staging/production split unless you say yes. A second `add domain` is an alias on the first app (`www`, `api.example.com`, …): one Ingress, one certificate. A separate API process is `buidl add api --host api.example.com`.

## After the first deploy

```sh
buidl status
buidl watch
buidl logs
buidl deploy --dry-run
buidl rollback
```

`destroy` requires `-e` only when overlays exist.

## How a release works

1. BuildKit builds the image. With a registry it pushes; with no registry, `deploy` copies a docker-save archive onto each server, imports it into containerd, and deletes the tars. Nothing is stored in a local Docker image store.
2. The deploy pins that image by digest. A pod restart cannot pick up different bytes.
3. A hidden `promote` ships an existing digest to another environment. It does not rebuild.
4. Release history lives in the cluster, so `status --history` and `rollback` work from any machine.

`deploy --dry-run` dry-runs against the API server, so the diff uses the same defaulting and admission a real apply would. `--detailed-exitcode` returns 2 when something would change (cluster or app), which is how a pipeline can require approval only when needed.
