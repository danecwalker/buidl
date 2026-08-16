# buidl

Build container images without a Docker daemon and deploy them to Kubernetes as immutable, digest-pinned releases.

```
buidl init
buidl add server 203.0.113.10 --email you@example.com
buidl add domain example.com
buidl deploy
```

That is the happy path. You should not need to edit `buidl.yaml` for it — the commands write the file, and omitted settings have safe defaults. `buidl init` is a short setup wizard: it asks whether you want GitHub Actions, then staging, then review apps, and writes the workflow and overlays for you. Omit `--registry` to build locally and copy the image onto your servers; pass `--registry ghcr.io/myorg` to push instead. One live app, blue-green updates, no staging/production split unless you say yes. `deploy --dry-run`, `rollback`, `watch`, and `destroy` are there when you need them. `destroy` requires `-e` only when overlays exist.

`deploy` can also install k3s or RKE2 on machines you already have. Creating those machines is not buidl's job. Use OpenTofu, Terraform, Ansible, or a cloud console, then `buidl add server`.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/danecwalker/buidl/main/install.sh | bash
```

That detects the platform, shows download progress, and verifies the SHA-256 against `checksums.txt` from the same GitHub release. The binary lands in `~/.local/bin` so later `buidl update` does not need sudo. If that directory is not on PATH, the installer links `/usr/local/bin/buidl` at it (sudo once). Set `BUIDL_INSTALL_DIR` to install somewhere else. Later upgrades are `buidl update` — run it without sudo.

linux and darwin, amd64 and arm64. The binaries are static (`CGO_ENABLED=0`) and built with `-trimpath`.

With Go 1.25+:

```sh
go install github.com/danecwalker/buidl/cmd/buidl@latest
```

`go install` does not stamp the version, so the binary reports `dev`. Use the installer or `make install` if you need `buidl --version` to match a tag. `buidl update` will replace a `dev` binary with the latest release.

From source:

```sh
git clone https://github.com/danecwalker/buidl.git
cd buidl
make build      # ./bin/buidl
make install    # $GOPATH/bin/buidl, version stamped from git describe
```

`buidl --version` reports the release tag on published binaries and the git describe string on a `make install` build.

## Quick start

Needs a kubeconfig and a BuildKit endpoint (see [Requirements](#requirements)).

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

`init` detects Go, Node, Python, Ruby, Rust, and static sites. If there is no Dockerfile it writes a multi-stage one. It also scaffolds `.buidl/` (secrets + hooks). On a terminal it asks about GitHub Actions, staging, and review apps. Scripts pass `--ci`, `--staging`, `--preview`, or `--no-ci` instead.

A second `add domain` is an alias on the first app (`www`, `api.example.com`, …): one Ingress, one certificate. A separate API process is `buidl add api --host api.example.com`.

## How a release works

1. BuildKit builds the image. With a registry it pushes; with no registry, `deploy` copies a docker-save archive onto each server, imports it into containerd, and deletes the tars. Nothing is stored in a local Docker image store.
2. The deploy pins that image by digest. A pod restart cannot pick up different bytes.
3. A hidden `promote` ships an existing digest to another environment. It does not rebuild.
4. Release history lives in the cluster, so `status --history` and `rollback` work from any machine.

`deploy --dry-run` dry-runs against the API server, so the diff uses the same defaulting and admission a real apply would. `--detailed-exitcode` returns 2 when something would change (cluster or app), which is how a pipeline can require approval only when needed.

## Configuration

A minimal file is enough to deploy:

```yaml
app: web
image: ghcr.io/acme/web
```

That gives you a HorizontalPodAutoscaler (CPU 70%, bounds from the fleet or a 1–4 fallback), port 8080, `/livez` `/readyz` `/startupz` probes, a blue-green update, a non-root pod with all capabilities dropped, a namespace named after the app (created on first deploy), and an imagePullSecret copied from your local Docker login so the cluster can pull the image you just pushed. Set `replicas` to pin a static count. Set `deploy.strategy.type: rolling` to keep a rolling update. Set `createNamespace: false` if you manage the namespace yourself. Preview environments stay at one replica.

A more complete file:

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

# Opt in with `buidl environment new`. Init does not write these.
environments:
  staging:
    proxy: {host: staging.acme.com}

  production:
    deploy:
      autoscale: {min: 5, max: 20}   # optional; omit to size from the fleet

  preview:
    deploy:
      kubernetes:
        namespace: web-preview-${BUIDL_SLUG}
        createNamespace: true
        ephemeral: true
    proxy:
      host: ${BUIDL_SLUG}.preview.acme.com
```

Most apps never need the `environments` block. Add one when you want a second target (GitHub Actions staging, a preview per PR):

```sh
buidl environment list
buidl environment new staging --host staging.example.com
buidl environment new production --host example.com
buidl environment set staging
```

The first overlay you create becomes `defaultEnvironment`. Production is never implied from an empty `-e` when several overlays exist and no default is set. Environments deep-merge onto the base. Maps merge key by key. Sequences are replaced, so `platforms: [linux/arm64]` in an overlay means exactly that list.

`environment` (alias `env`) edits the overlays. It does not create or destroy cluster objects.

### Variables

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

A preview environment is disposable. `buidl destroy -e preview` deletes its namespace. Long-lived environments keep their namespace and any accessories; only the app objects are removed, and production also requires `--force`.

`destroy --stale 7d` is the backstop for a missed close event: it deletes preview namespaces older than the duration. On a single-target file (no overlays), `buidl destroy` does not need `-e`.

### Health checks

Your app should serve three HTTP endpoints. They are not interchangeable. A deploy succeeds only after readiness passes.

**`GET /startupz` — has this process finished booting?**

Kubernetes will not run the other two until this succeeds. Return 200 once the listen loop is up. A slow migration or cache warm belongs here, not on liveness: a slow boot is not a deadlock.

**`GET /readyz` — can this instance accept traffic?**

The rollout waits on this. Return 200 only when you can serve a real request, including required backends (a Postgres ping). Return 503 to take yourself out of the load balancer without being restarted.

**`GET /livez` — is this process still making progress?**

A failure restarts the container. Keep it cheap: if the process is running, return 200. Do not check Postgres. A database blip would restart every replica at once.

HTTP 200–399 is success. Return a tiny body (`ok`). Probes hit the container's named `http` port.

If you already have one endpoint (Rails `/up`, Kamal), point all three at it:

```yaml
deploy:
  healthcheck:
    path: /up
```

Override a single probe with `readiness`, `liveness`, or `startup`. Workers set `command` instead of HTTP paths. buidl does not guess another path if these fail: a timed-out deploy names the configured readiness URL.

Postgres and Redis accessories get `pg_isready` / `redis-cli ping` with the same timing split: a frequent startup probe, a stricter readiness check, and a more forgiving liveness check.

### Secrets

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

`buidl variable list` (or `var` / `vars`) prints each variable, its kind, and where it came from. Secrets show as `set, N chars`, never the value. Accessory secrets such as `POSTGRES_PASSWORD` are resolved from the same files even when they are not listed under the app's `env.secret`; they are not injected into the app.

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

### Private registries

buidl needs credentials to push. The cluster needs its own credentials to pull. Your local `docker login` does not reach the kubelet.

`registry.createPullSecret` defaults to true, and `buidl init` writes it. The same credential BuildKit pushed with is copied into the cluster as an imagePullSecret. GHCR packages are private until you flip them, so without this the first deploy builds, pushes, then dies on `ErrImagePull`.

```yaml
registry:
  createPullSecret: true          # the default; false keeps credentials off the cluster
  # or reference a secret you already manage:
  # pullSecret: my-registry-creds
```

Push credentials come from the standard Docker config (`docker login`, `gcloud auth configure-docker`, `docker/login-action`). Set `createPullSecret: false` for a public image or when the nodes already have a `registries.yaml`.

### Stateful apps

Postgres and Redis are apps in the stack, the same as web or api. Add them with a command:

```sh
buidl add postgres
buidl add redis
```

That writes `type: postgres` (or `redis`) under `accessories` and generates `POSTGRES_PASSWORD` plus `DATABASE_URL` into `.buidl/secrets`. Image, port, volume and mount path are filled at load. A first `buidl deploy` creates any stateful app that is not already in the cluster. Later deploys leave existing ones alone — including ones that have drifted — so shipping a web change cannot restart a database. Reconcile one with `buidl deploy postgres` (it prompts before anything that restarts a pod).

```yaml
accessories:
  postgres:
    type: postgres
    # image, port, storage, and POSTGRES_PASSWORD are defaulted
```

An explicit `image` / `storage` still wins. Untyped accessories with a full spec keep working.

Each one becomes a StatefulSet plus a headless Service. Inside the namespace, `postgres` resolves as `<app>-postgres`. Typed (or well-known) Postgres and Redis images get exec probes so a first boot is covered by startup rather than a hair-trigger liveness restart.

Images are not digest-pinned (buidl did not build them). They are never deleted: removing one from `buidl.yaml` stops managing it. You delete the StatefulSet and volume yourself.

They have unit tests and render through the real Kubernetes scheme. They have not been applied to a real cluster yet. Treat the first one as an experiment.

### Hooks

Executables in `.buidl/hooks`. Each one gets the release identity and every resolved secret.

| Hook | When | Failure aborts the deploy? |
|---|---|---|
| `pre-build` | before the image is built | yes |
| `post-build` | after the image is pushed (`BUIDL_DIGEST` set) | yes |
| `pre-deploy` | after preflight, before apply | yes |
| `post-deploy` | after the release is healthy | no |
| `deploy-failed` | after a failed deploy | no |

`pre-deploy` is the usual place for migrations: the image exists, nothing is serving yet, and the hook can use a credential the app never sees.

Hooks also get `BUIDL_APP`, `BUIDL_ENV`, `BUIDL_RELEASE`, `BUIDL_DIGEST`, `BUIDL_IMAGE`, `BUIDL_NAMESPACE`, `BUIDL_GIT_SHA`, `BUIDL_GIT_BRANCH`, `BUIDL_ACTOR`, `BUIDL_URL`, `BUIDL_VERSION`, and `BUIDL_HOOK=1`. A hook that exists but is not executable is reported, not skipped. `buidl hooks` lists what is enabled.

## Clusters

buidl never creates VMs, networks, firewalls, or load balancers. Once the machines exist, list them:

```yaml
infra:
  ssh:
    user: root              # keyPath optional; ssh-agent is used by default
  kubernetes:
    distribution: k3s       # or rke2
    version: v1.33.5+k3s1   # pin anything you intend to keep
    controlPlaneEndpoint: k8s.acme.com   # required for more than one control plane
    disable: [traefik]
  addons:
    buildkit: true
    certManagerEmail: ops@acme.com   # enables cert-manager when proxy.ssl is set
    # metricsServer is turned on automatically when deploy.autoscale is set
    # and the distribution does not already bundle it (k3s does).
  servers:
    - {host: 203.0.113.1, role: control-plane, privateIP: 10.0.0.1}
    - {host: 203.0.113.2, role: control-plane, privateIP: 10.0.0.2}
    - {host: 203.0.113.3, role: control-plane, privateIP: 10.0.0.3}
    - {host: 203.0.113.10, role: worker, labels: {pool: web}}
    - {host: 203.0.113.11, role: worker, labels: {pool: gpu}, taints: ["gpu=true:NoSchedule"]}
```

There is no separate "create the cluster" command. `buidl deploy --dry-run` inspects the servers and includes any Kubernetes install they need. If the cluster is already there, it also fetches its kubeconfig so the application diff can run. `buidl deploy` converges the cluster, then ships the app.

```sh
buidl deploy --dry-run -e production
buidl deploy  -e production
```

Use `deploy --skip-cluster` against a cluster you manage yourself.

The `cluster` commands are for inspection and teardown:

| Command | What it does |
|---|---|
| `cluster inventory` | resolved fleet, and which machine bootstraps |
| `cluster status` | node health, including machines that never joined |
| `cluster kubeconfig` | fetch credentials and merge into `~/.kube/config` |
| `cluster reset` | uninstall Kubernetes. Leaves the servers running. Type the environment name, or pass `--yes`. |

### Dual-stack (IPv4 and IPv6)

New clusters are dual-stack. The defaults are:

```yaml
infra:
  kubernetes:
    clusterCIDR: 10.42.0.0/16,fd00:42::/56
    serviceCIDR: 10.43.0.0/16,fd00:43::/112
```

Pod and service IPv6 is ULA (`fd00::/8`), so it does not consume the host's public `/64`. k3s also gets `flannel-ipv6-masq`, which SNATs pod IPv6 to the node's public address. RKE2 gets the same CIDRs and does not get the flannel key.

This is required for public TLS. Let's Encrypt prefers IPv6 whenever a name has an AAAA. An IPv4-only ingress on a host with a public AAAA fails HTTP-01 and Traefik keeps serving its default certificate.

DNS and the cloud firewall have to match:

- A record: the server's IPv4
- AAAA record: the server's IPv6 (on Hetzner, `2a01:...::1` from the assigned `/64`, not the `/64` itself)
- 80/443 open from `0.0.0.0/0` and `::/0`

`privateIP` still pins `node-ip` to the private IPv4. If the host also has a global IPv6, buidl appends it so ServiceLB publishes both families.

Opt out by setting both CIDRs to IPv4-only. Mixed families (IPv6 pods, IPv4-only services) are rejected.

An existing IPv4-only cluster cannot pick this up in place. Rewriting `cluster-cidr` on a running node crash-loops flannel. Rebuild it:

```sh
buidl cluster reset -e production    # type the environment name, or pass --yes
buidl deploy -e production
```

`cluster reset` uninstalls Kubernetes and leaves the machines running. The next `deploy` bootstraps a new dual-stack cluster.

Only `infra.provider: static` is implemented. Addresses go in `buidl.yaml`. The inventory interface exists so a later provider can read `tofu output -json` or an Ansible inventory without changing the cluster code.

### OpenTofu owns the machines

`examples/hello/infra` is a complete split: OpenTofu creates a Vultr VM, firewall, and SSH key. buidl never calls a cloud API.

```sh
cd examples/hello/infra
tofu init
tofu apply
tofu output -raw ipv4
# put that address in examples/hello/buidl.vultr.yaml, point DNS at it
buidl deploy -e vultr -f examples/hello/buidl.vultr.yaml
```

`examples/hello/infra/ha` does the same for three control-plane nodes. HA join ordering (one control plane at a time) and `buidl promote` are implemented. That HA example has not been run against real machines.

### Cluster behaviour

- The first control-plane server initializes the cluster. Extra control planes join one at a time so two etcd members never join at once. Workers join in parallel.
- Re-runs converge. A healthy node whose rendered config matches is left alone (`up-to-date`). Other actions are `bootstrap`, `join-control-plane`, `join-worker`, `reconfigure`, `upgrade`, or `skipped`.
- If no server can be reached, that is an error, not "no changes".
- Two control planes is worse than one (twice the failure surface, still zero fault tolerance). buidl warns. More than one control plane without `controlPlaneEndpoint` is an error.
- Single-node clusters still use embedded etcd, so they can grow later.
- `privateIP` sets `node-ip` so cluster traffic stays on the private network. If the host also has a global IPv6, it is appended so ServiceLB can publish both families.
- New clusters are dual-stack. See [Dual-stack](#dual-stack-ipv4-and-ipv6). Existing IPv4-only clusters need `cluster reset` then `deploy`, not a live CIDR rewrite.
- Host keys are checked by default. Unknown keys fail with instructions to run `ssh-keyscan`, or set `infra.ssh.acceptNewHostKeys: true` for trust-on-first-use. A mismatched key always fails.
- The join token is never printed, is written `0600`, and is generated with `crypto/rand` when you do not supply one.
- Remote commands are single-quote escaped. File content is streamed over stdin, not pasted into `echo`.
- In-cluster BuildKit (`addons.buildkit: true`) is rootless. Point builds at it with `build.addr: tcp://buildkitd.buidl-system:1234`.
- After converging, deploys use the kubeconfig context buidl created (`<app>-<environment>`), not whatever happens to be current.
- A version `upgrade` re-runs the installer in place and restarts the unit, one control plane at a time. It does not cordon or drain first. For production, drain the nodes yourself or accept the restart.

## Plan and deploy output

`buidl deploy --dry-run` reports each object, the fields that change, and the runtime effect:

```
    environment  production
    release      a1b2c3d-tjnz3d
    image        sha256:bbbbbbbbbbbb

       KIND        NAME  CHANGES                                              EFFECT
    ~  Deployment  web   image: sha256:aaaa -> sha256:bbbb, replicas: 3 -> 5  replaces 5 instances
    +  Ingress     web                                                        publishes externally
       Service     web

    plan: 1 to create, 1 to update, 1 unchanged
```

| Effect | Meaning |
|---|---|
| `replaces N instances` | a pod-template field changed (image, env, resources, port, secrets) |
| `scales to N` | only the replica count changed |
| `no restart` | inert edit (metadata, strategy) |
| `switches live traffic` | Service selector flip (blue-green cutover) |
| `publishes externally` | a new Ingress |

Unchanged objects are listed too. `--detailed` adds the full YAML diff. Secret changes are reported by key name only (`values changed: DATABASE_URL`). Values never appear in plan output.

`buidl deploy` ends with what changed, what is running, and where the time went. If apply fails partway through, it names what already landed and says to re-run (apply is idempotent). Terminal pod states (`ImagePullBackOff`, `CrashLoopBackOff`, missing Secret) are caught in seconds, with the failing container's logs, instead of waiting out the deploy timeout.

`-o json` emits newline-delimited events. Every line decodes into the same `Event` type. Release ID, digest, and URL are also exported as CI step outputs.

## Commands

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

Also implemented, hidden from default help: `environment`, `variable`, `cluster`, `promote`, `build`, `manifest`, `config`, `hooks`, `plan`, `releases`, `accessory`, `add app`.

Global flags: `-e/--env`, `-f/--config`, `-o/--output {auto,pretty,plain,json}`, `-v/--verbose`, `--no-color`, `--timeout`.

Useful deploy flags: `--dry-run`, `--detailed`, `--detailed-exitcode`, `--auto-rollback`, `--skip-cluster`, `--skip-build --digest sha256:...`, `--allow-dirty`, `--yes`.

Useful destroy flags: `--yes`, `--dry-run`, `--stale 7d`, `--force` (production).

Exit codes: `0` success, `1` failure, `2` changes detected (`deploy --dry-run --detailed-exitcode`), `3` invalid configuration.

## CI

Output is plain in CI, colored on a terminal, or newline-delimited JSON with `-o json`. Warnings and errors become CI annotations. A dirty working tree warns locally and fails in CI.

`buidl init` asks whether you want GitHub Actions. A yes writes a workflow that deploys the app on every push to `main`. If you also want staging, the workflow deploys staging on push and promotes that digest to production. Review apps add a preview per pull request and tear it down when the PR closes. `--ci`, `--staging`, and `--preview` answer the same questions without a prompt.

## Requirements

- A BuildKit endpoint, unless `build.driver: prebuilt`. Discovery order: `build.addr`, `$BUILDKIT_HOST`, a rootless socket, `/run/buildkit/buildkitd.sock`, then a Docker/Podman/nerdctl container named `buildkitd` (or any running `moby/buildkit`, including a Buildx builder). If none of those exist, buidl creates `moby/buildkit:v0.25.1` as `buildkitd`.
- Kubernetes 1.30+, for server-side apply and `preStop.sleep`.
- cert-manager if `proxy.ssl: true`. Set `infra.addons.certManagerEmail` and buidl installs it.
- An ingress controller if `proxy.host` is set.
- metrics-server if `deploy.autoscale` is set. buidl installs it on RKE2, or when k3s's bundled copy has been disabled.
- For public TLS with an AAAA record: 80 and 443 reachable on IPv6. Let's Encrypt validates over IPv6 first.

Writes use server-side apply with field manager `buidl`. Per app and environment that is a `ServiceAccount`, `Deployment`, `Service`, and optionally `Secret`, `Ingress`, `HorizontalPodAutoscaler`, `PodDisruptionBudget`, `Namespace`. Objects get `app.kubernetes.io/*` labels and `buidl.dev/*` annotations (release, digest, commit, actor, timestamp).

## Examples

All under [`examples/hello`](examples/hello): a small Go app with failure switches (`FAIL_READINESS`, `CRASH_ON_BOOT`, `BOOT_DELAY`) so you can prove the unhappy path.

| File | What it is for |
|---|---|
| `buidl.yaml` | existing local cluster. Used by the acceptance suite. |
| `buidl.vm.yaml` | two machines over SSH. Installs k3s, joins a worker, deploys. |
| `buidl.vultr.yaml` | one public server, Ingress, cert-manager, TLS. |
| `buidl.ha.yaml` | three control planes and a staging-to-production promote. |
| `infra/` | OpenTofu that creates the Vultr machines those last two configs expect. |

Against an existing cluster:

```sh
export DEMO_SECRET=any-value-for-testing
buidl deploy -e local -f examples/hello/buidl.yaml
FAIL_READINESS=1 buidl deploy -e local -f examples/hello/buidl.yaml --auto-rollback
```

Against two VMs you already have, after putting their addresses in the file:

```sh
buidl deploy --dry-run -e vm -f examples/hello/buidl.vm.yaml
buidl deploy -e vm -f examples/hello/buidl.vm.yaml
```

## Status

The Kubernetes deploy backend and the k3s installer have been run against real clusters and real machines. The acceptance suite (`test/acceptance/run.sh`) covers build, push, apply, health gating, crash-loop detection, auto-rollback, hooks, pull secrets, and the inspection commands.

Not yet run against real infrastructure:

- RKE2
- version upgrades on a live cluster
- `cluster reset`
- blue-green cutover
- accessories
- multi-arch builds
- the three-node HA example and `promote` on that topology

Not implemented: inventory providers other than `static`, and `deploy.target` values other than `kubernetes`.

## Development

```sh
make test        # unit tests. No cluster, no network.
make lint
make build
make acceptance  # real cluster and registry
```

```sh
export DEMO_SECRET=any-value-for-testing
make acceptance
KEEP=1 ./test/acceptance/run.sh healthy
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE)
