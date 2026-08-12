# buidl

A CLI for building and deploying applications — Kubernetes, cloud, and bare metal — with Vercel-like ergonomics.

```
buidl init                                    detect the project, write buidl.yaml
buidl plan -e production                      show everything a deploy would change
buidl deploy -e staging                       converge the cluster, build, roll out
buidl promote --from staging --to production   deploy staging's exact image
buidl rollback -e production                  revert to the previous release
```

## What makes it different

**No Docker daemon.** Builds go through [BuildKit](https://github.com/moby/buildkit)'s gRPC API directly and export straight to the registry. Nothing is written to a local image store, so there is no separate `docker push` to fail. BuildKit can run rootless or as a Pod in the target cluster, which means a CI runner needs no privileged container.

**Immutable, digest-pinned releases.** Every deploy mints a release ID and pins the image by digest. A pod restart can never pick up different bytes than the ones you deployed. `promote` re-deploys an existing digest rather than rebuilding, so what you tested in staging is byte-identical to what runs in production — a rebuild could resolve a new base layer or dependency and quietly differ.

**Deploys that fail loudly and early.** Preflight checks cluster reachability, RBAC, secret presence, and image existence *before* anything is applied. Rollout waiting detects terminal pod states (`ImagePullBackOff`, `CrashLoopBackOff`, missing Secret) in seconds and prints the failing container's logs, instead of timing out after five minutes with "progress deadline exceeded".

**Real plans.** `buidl plan` dry-runs against the API server, so the diff comes from the same defaulting and admission logic a real apply uses — not a client-side guess. It reports changes *field by field* with their runtime effect, so you can tell a pod-replacing change from an inert one before you run it. `--detailed-exitcode` returns 2 when changes exist, so a pipeline can require approval only when something would actually change.

**Deploys that account for themselves.** Every run ends with what changed, what is running now, and where the time went — see [Reporting](#reporting).

**The cluster is the source of truth.** Release history lives in the cluster's own revision records, so `releases` and `rollback` work from any machine or CI run, with no external state store to keep in sync.

**Bare servers to deployed app.** buidl installs Kubernetes (k3s or RKE2) on servers you already have, joins them into one cluster, and hands you a kubeconfig. It does *not* provision infrastructure — see below.

## Clusters

buidl draws a deliberate line: **it never provisions infrastructure.** Creating VMs, networks, firewalls and load balancers is the job of OpenTofu, Terraform, Ansible or your cloud console — tools that own that state and do it well. buidl's job starts once the machines exist.

Provision however you like, then list what you got:

```yaml
infra:
  ssh:
    user: root                    # keyPath optional; ssh-agent is used by default
  kubernetes:
    distribution: k3s             # or rke2
    version: v1.34.1+k3s1         # pin it for anything you intend to keep
    controlPlaneEndpoint: k8s.acme.com   # required for HA
    disable: [traefik]
  addons:
    buildkit: true                # so `buidl deploy` has a builder immediately
    certManager: true
    certManagerEmail: ops@acme.com
    metricsServer: true
  servers:
    - {host: 203.0.113.1, role: control-plane, privateIP: 10.0.0.1}
    - {host: 203.0.113.2, role: control-plane, privateIP: 10.0.0.2}
    - {host: 203.0.113.3, role: control-plane, privateIP: 10.0.0.3}
    - {host: 203.0.113.10, role: worker, labels: {pool: web}}
    - {host: 203.0.113.11, role: worker, labels: {pool: gpu}, taints: ["gpu=true:NoSchedule"]}
```

**There is no separate "create the cluster" command.** `buidl plan` inspects the servers and includes whatever Kubernetes installation the fleet needs; `buidl deploy` converges the cluster and then ships the app. A fresh set of servers and an established cluster take the same command, because the plan already knows the difference.

```sh
buidl plan       # inspect servers + show cluster and app changes
buidl deploy     # install/join what's missing, then build and roll out
```

The `cluster` subcommands are for inspection and teardown only:

| Command | Purpose |
|---|---|
| `cluster inventory` | Print the resolved fleet and which machine bootstraps |
| `cluster status` | Node health, including machines that never joined |
| `cluster kubeconfig` | Fetch credentials and merge into `~/.kube/config` |
| `cluster reset` | Uninstall Kubernetes (destructive; leaves servers running) |

Use `deploy --skip-cluster` to skip server inspection entirely and deploy to a cluster you manage yourself.

### How it behaves

**Ordering follows etcd, not convenience.** The first control-plane server initializes the cluster. Additional control planes join **one at a time** — adding two etcd members concurrently can cost the cluster its quorum. Workers join in parallel, since they hold no cluster state.

**Re-running converges.** Servers already joined with matching configuration are left alone. The rendered node config is compared before writing, so a healthy node's service is not restarted on every run (which would cause a brief API outage each time). Each server is reported as one of `bootstrap`, `join-control-plane`, `join-worker`, `reconfigure`, `upgrade`, `up-to-date`, or `skipped`.

**Version changes are detected.** The distribution version is not part of the node config file, so a changed `kubernetes.version` is caught by comparing the installed version directly — otherwise it would compare equal and be silently reported as up to date. An unpinned version never drifts, since you asked for "current stable".

**A fleet it cannot inspect is a hard error, not "no changes".** If no server can be reached, buidl says the cluster's state is unknown and exits non-zero. Reporting "no changes" there would read as a healthy cluster, which is the most dangerous thing it could say.

**Topology mistakes are caught.** A 2-member control plane is *less* available than a single node — it doubles the failure surface while still tolerating zero failures — so buidl warns. More than one control plane without a `controlPlaneEndpoint` is a hard error: every node would hard-code the bootstrap machine's address, and losing it would break future joins even though the cluster survived.

**Embedded etcd always, even for one node.** The alternative (sqlite) cannot later accept additional control planes without a migration, so a single-node cluster stays growable.

**Everything is driven by a config file, not flags.** Re-running the installer picks up the same file, so runs converge rather than producing differently-flagged units — and the join token never appears in the process list or shell history.

**`privateIP` matters.** On most clouds the public address is NAT'd or metered. Setting it pins `node-ip` so intra-cluster traffic uses the private network instead of routing over the internet.

### Security

- **Host keys are verified by default.** Bootstrap installs root-level software and copies a cluster-admin credential back, so an unverified connection is a machine-in-the-middle window at the worst possible moment. Unknown keys produce an error explaining how to pre-seed them (`ssh-keyscan`), or set `infra.ssh.acceptNewHostKeys: true` to trust-on-first-use. A *mismatched* key is always a hard failure.
- **No shell injection.** Every value interpolated into a remote command is single-quote escaped; file content is streamed over stdin rather than pasted into an `echo`. The test suite round-trips ~35 hostile inputs through a real shell.
- **The join token is never printed** (`cluster plan --show-config` redacts it), is written `0600`, and is generated with `crypto/rand` when not supplied.
- **The on-node kubeconfig is `0640`**, not world-readable, and the local merge is chmod'd `0600`.
- **In-cluster BuildKit is rootless**, not privileged. A build runs the least trustworthy code in the system.
- **`cluster reset` requires typing the environment name** to confirm, and refuses to run non-interactively without `--yes`.

### The addon that closes the loop

`addons.buildkit: true` installs a rootless in-cluster buildkitd. Once the cluster exists, point builds at it:

```yaml
build:
  addr: tcp://buildkitd.buidl-system:1234
```

Now `buidl deploy` has a builder with no Docker installed anywhere, no privileged container, and no CI runner to configure.

### Cluster and app in one plan

With `infra` configured, `buidl plan` reports both layers, and `--detailed-exitcode` returns 2 if *either* would change:

```
distribution  k3s v1.34.1+k3s1
topology      HA: 3 control-plane, 2 worker
registration  https://k8s.acme.com:6443

HOST          ROLE           SYSTEM        ACTION              DETAIL
203.0.113.1   control-plane  Linux/x86_64  bootstrap           will initialize the cluster
203.0.113.2   control-plane  Linux/x86_64  join-control-plane  will join as a control-plane member
203.0.113.3   control-plane  Linux/x86_64  join-control-plane  will join as a control-plane member
203.0.113.10  worker         Linux/x86_64  join-worker         will join as a worker

plan: 4 of 4 server(s) need changes
```

When the cluster does not exist yet there is nothing to diff the application against, so the plan reports the cluster work alone and says the app plan becomes available once the cluster is up.

After converging, buidl points the deploy at the kubeconfig context it just created (`<app>-<environment>`) rather than whatever context happened to be current — which, right after building a new cluster, is almost never what was meant.

## Influences

The configuration model and ergonomics come from [Kamal](https://kamal-deploy.org) — one declarative file, named environments overlaying a common base, accessories, a simple proxy block, one-command rollback, and the "bring your own servers" premise. The release model comes from Vercel — immutable deploys, per-branch previews, promotion instead of rebuilding. The execution layer is Kubernetes-native: server-side apply, rollout gating on health checks, HPA-aware scaling.

Where buidl differs from Kamal: releases are immutable and digest-pinned rather than tag-based, promotion between environments is a first-class operation, and the runtime is Kubernetes instead of containers on hosts — which is what makes scaling, rolling updates and multi-node scheduling the platform's job rather than the tool's.

## Install

```sh
go install github.com/danewalker/buidl/cmd/buidl@latest
```

Or build from source:

```sh
make build      # ./bin/buidl
make install    # $GOPATH/bin/buidl
```

## Quick start

```sh
cd my-app
buidl init --registry ghcr.io/myorg
# review buidl.yaml, set proxy.host
buidl config validate
buidl deploy -e staging
```

`init` detects Go, Node, Python, Ruby, Rust, and static sites; generates a hardened multi-stage Dockerfile if you don't have one; scaffolds a gitignored secrets file and a GitHub Actions workflow.

## Configuration

A minimal config is four lines. Everything else has a safe default.

```yaml
app: web
image: ghcr.io/acme/web
```

That yields: 1 replica on port 8080, a `/up` readiness probe gating the rollout, a zero-downtime rolling update (`maxUnavailable: 0`), a non-root pod with all capabilities dropped, and a namespace named after the app.

A realistic config:

```yaml
version: 1
app: web
image: ghcr.io/acme/web

build:
  driver: buildkit          # or "prebuilt" to deploy an existing image
  dockerfile: Dockerfile
  platforms: [linux/amd64]
  cache: registry           # survives ephemeral CI runners
  secrets:
    npm_token: env:NPM_TOKEN   # mounted via BuildKit, never in a layer

deploy:
  target: kubernetes
  port: 3000
  replicas: 3
  healthcheck:
    path: /up               # the rollout gate
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {memory: 512Mi}
  strategy:
    type: rolling           # rolling | bluegreen | recreate
    maxUnavailable: "0"
  drainTimeout: 30s
  deployTimeout: 5m

env:
  clear:
    LOG_LEVEL: info
  secret: [DATABASE_URL]    # names only; values resolved at deploy time

proxy:
  host: acme.com
  ssl: true                 # cert-manager issues the certificate

environments:
  staging:
    deploy: {replicas: 1}
    proxy: {host: staging.acme.com}

  production:
    deploy:
      replicas: 5
      autoscale: {min: 5, max: 20, cpuPercent: 70}

  preview:
    deploy:
      kubernetes:
        namespace: web-preview-${BUIDL_SLUG}
        createNamespace: true
    proxy:
      host: ${BUIDL_SLUG}.preview.acme.com
```

### Environment overlays

Environments deep-merge onto the base. Maps merge key by key; **sequences are replaced wholesale**, so `platforms: [linux/arm64]` in an overlay means exactly that, not "additionally arm64".

### Variables

`${VAR}`, `${VAR:-default}`, and `${VAR:?why it's needed}` are interpolated from the environment. buidl injects:

| Variable | Value |
|---|---|
| `BUIDL_ENV` | selected environment name |
| `BUIDL_SHA` / `BUIDL_SHORT_SHA` | commit sha |
| `BUIDL_BRANCH` | branch name (recovered from CI on detached HEAD) |
| `BUIDL_SLUG` | DNS-safe branch slug, or `pr-<n>` in CI |
| `BUIDL_GIT_TAG` | exact-match git tag, if any |
| `BUIDL_PR` | pull request number in CI |

`BUIDL_SLUG` is what makes per-branch preview environments need zero configuration.

Interpolation runs on the parsed document, not the raw bytes, so a value containing YAML metacharacters can never corrupt the file's structure.

### The `.buidl` directory

Modeled on Kamal's `.kamal/`, because the split it encodes is genuinely useful — one committed file that declares *which* secrets exist and where they come from, and per-environment files that are gitignored because people inevitably paste literals into them.

```
.buidl/
  secrets-common          committed — indirections only, no values
  secrets                 gitignored — all environments
  secrets.production      gitignored — one environment
  hooks/
    pre-deploy            executable; runs migrations
    pre-deploy.sample     scaffolded, inert until you chmod +x
```

`buidl init` scaffolds this and adds `.buidl/secrets` and `.buidl/secrets.*` to `.gitignore` — **not** `.buidl/`, since `secrets-common` and the hooks are meant to be committed.

### Secrets

`env.secret` lists **names only**. Values resolve at deploy time, lowest precedence first:

| Source | Committed? | Notes |
|---|---|---|
| `.env`, `.env.<environment>` | usually | only when `env.dotenv: true` |
| `.buidl/secrets-common` | **yes** | indirections only: `DATABASE_URL=$PROD_DATABASE_URL` |
| `.buidl/secrets` | no | all environments |
| `.buidl/secrets.<environment>` | no | one environment |
| the process environment | — | **always wins** |

The process environment outranks every file so CI injection is never silently overridden by a stale file on a developer's machine. A `$VAR` value indirects through the environment, which is what makes `secrets-common` safe to commit: it declares the binding, not the secret. An *unresolved* indirection is treated as missing rather than deployed as the literal string `$VAR`.

Values are written to a Kubernetes Secret, never to `buidl.yaml`. A checksum of the resolved secrets is annotated onto the pod template, so **changing a secret value triggers a rollout** — without it Kubernetes would leave pods running the stale value.

`buidl env list` shows every variable, its kind, and where it resolved from. Secrets are reported as `set, N chars` — never the value.

For externally managed secrets (External Secrets, Vault), use `env.secretRefs` to mount existing Secrets by name.

### Reading existing `.env` files

Most projects already have `.env.production`. Set `env.dotenv: true` and buidl will read `.env` and `.env.<environment>` for the values of names you declared:

```yaml
env:
  dotenv: true
  secret: [DATABASE_URL, STRIPE_KEY]
```

Two deliberate constraints:

- **Only declared names are deployed.** Dotenv files supply *values*, not the list of what to ship, so a stray local variable can't silently become part of a release. Undeclared names found in those files are reported by `env list` as "found but NOT deployed", which is the difference between "works locally" and "works in the cluster".
- **`.env.local` and `.env.<environment>.local` are never read.** By convention those are gitignored machine-local dev config; deploying one would ship a developer's `localhost` database URL to production. buidl warns when it skips one, so the omission reads as a decision rather than a bug.

### Private registries

buidl needs credentials to **push**; the cluster needs its own to **pull**. Conflating those is the usual cause of "it built fine but the pods won't start" — your `docker login` does not reach the kubelet.

```yaml
registry:
  createPullSecret: true      # buidl creates and maintains the imagePullSecret
  # or reference one you already manage:
  # pullSecret: my-registry-creds
```

`createPullSecret` copies a credential from this machine into the cluster, which is a real trust decision — hence opt-in. When a pull fails with `unauthorized` and no pull secret exists, buidl says so and names the fix rather than leaving you with the kubelet's message.

### Lifecycle hooks

Executables in `.buidl/hooks`, named for the lifecycle point. Each receives the release's identity **and every resolved secret** in its environment.

| Hook | Runs | Failure aborts the deploy? |
|---|---|---|
| `pre-build` | before the image is built | yes |
| `post-build` | after the image is pushed (`BUIDL_DIGEST` set) | yes |
| `pre-deploy` | after preflight, before anything is applied | **yes** |
| `post-deploy` | after the release is healthy and serving | no |
| `deploy-failed` | after a failed deploy | no |

`pre-deploy` is the one that matters: a migration must run after the image exists but before the new release serves, and it needs a credential the application itself should not hold. Because hooks get secrets, the hook can use an owner-role `MIGRATIONS_DATABASE_URL` that the app never sees.

Hooks are given `BUIDL_APP`, `BUIDL_ENV`, `BUIDL_RELEASE`, `BUIDL_DIGEST`, `BUIDL_IMAGE`, `BUIDL_NAMESPACE`, `BUIDL_GIT_SHA`, `BUIDL_GIT_BRANCH`, `BUIDL_ACTOR`, `BUIDL_URL`. A hook that exists but isn't executable is reported rather than silently skipped — that's almost always a forgotten `chmod` after checkout. Run `buidl hooks` to see which are enabled.

## Reporting

The tool is built around four questions: what *will* happen, what *did* happen, what succeeded or failed, and what is running now.

### What will happen

`buidl plan` reports each object with the fields that change and the runtime effect:

```
    environment  production
    release      a1b2c3d-tjnz3d
    image        sha256:bbbbbbbbbbbb

       KIND        NAME  CHANGES                                              EFFECT
    ~  Deployment  web   image: sha256:aaaa… → sha256:bbbb…, replicas: 3 → 5  replaces 5 instances
    +  Ingress     web                                                        publishes externally
       Service     web

    plan: 1 to create, 1 to update, 1 unchanged
```

The `EFFECT` column is the part that matters for review. buidl knows which fields live in the pod template, so it distinguishes:

| Effect | Meaning |
|---|---|
| `replaces N instances` | A pod-template field changed (image, env, resources, port, secrets) — pods restart |
| `scales to N` | Only the replica count changed — existing pods keep running |
| `no restart` | An inert edit, e.g. metadata or a strategy field |
| `switches live traffic` | A Service selector flip — the blue-green cutover |
| `publishes externally` | A new Ingress |

Unchanged objects are listed too, so the plan accounts for everything buidl manages rather than only the deltas. `--detailed` adds the full YAML diff per object.

**Secret values are never printed.** Secret changes are reported by key name only (`values changed: DATABASE_URL`), because plan output routinely ends up in a pull request or a CI log — somewhere far more public than the Secret itself. There is a test that fails if a value ever leaks into this output.

### What was done, and what's running

`buidl deploy` ends with three blocks:

```
    Changes
    STATUS     KIND        NAME  CHANGES                         EFFECT
    applied    Deployment  web   image: sha256:aaa → sha256:bbb  replaces 2 instances
    unchanged  Service     web

    Running instances (2/2 ready)
    INSTANCE        PHASE    READY  RESTARTS  AGE  NODE    MESSAGE
    web-7d9f-abcde  Running  yes    0         30s  node-1
    web-7d9f-fghij  Running  yes    0         25s  node-2

    Deploy summary
        STEP                       TOOK    DETAIL
    ok  Building image             1m12s   sha256:bbbbbbbbbbbb
    ok  Preflight checks           420ms
    ok  Applying manifests         1.2s    2 changed, 1 unchanged
    ok  Waiting for health checks  38.4s   2/2 instances ready
    total 1m52s
```

Object-level changes are logged at normal verbosity, not behind `--verbose`: what a deploy did to your cluster is the primary record of the run.

### What failed

Apply is not atomic, so a failure partway through leaves a mixed state. buidl names it:

```
  warn: the deploy failed partway through; the namespace is in a mixed state
    already applied:
      - Secret/web-env
      - ServiceAccount/web
 error: failed to apply Deployment/web: forbidden: insufficient RBAC

    re-run `buidl deploy` once the cause is fixed; apply is idempotent,
    or inspect the current state with `buidl status`
```

Rollout failures are diagnosed rather than timed out: terminal pod states (`ImagePullBackOff`, `CrashLoopBackOff`, a missing Secret) are detected in seconds, and the failing container's last log lines are printed. While waiting, progress names the individual instances — `waiting for health checks: 2/3 instances ready [abcde=ready fghij=ready klmno=ContainerCreating]` — so you can see which one is lagging.

The closing summary marks the phase that failed, so a long CI log has one place to look.

### Cluster changes

Server convergence reports per machine, because a fleet change is not atomic either:

```
    Server changes
    STATUS  HOST          ROLE           ACTION              TOOK  DETAIL
    ok      203.0.113.1   control-plane  bootstrap           48s
    ok      203.0.113.2   control-plane  join-control-plane  31s
    FAILED  203.0.113.10  worker         join-worker         12s   dial tcp: connection refused
```

On failure it lists which machines were already changed and notes that re-running skips servers that are already correct.

### Machine-readable

`-o json` emits newline-delimited events. Every line decodes into the same `Event` type — there is a test that asserts this across every output primitive, so a consumer can `json.Unmarshal` line by line without special-casing. The closing summary is a single event carrying every step with its status, duration and error, so a CI job can assert on the shape of a run instead of scraping prose.

Release ID, digest and URL are also exported as CI step outputs.

## Commands

| Command | Purpose |
|---|---|
| `init` | Detect the project and scaffold config, Dockerfile, CI |
| `build` | Build and push an image; prints the digest |
| `deploy` | Converge the cluster if needed, then build, push, apply, wait for health |
| `plan` | Dry-run and show the diff, cluster included |
| `promote` | Deploy one environment's exact digest to another |
| `rollback` | Revert to the previous or a named release |
| `status` | What's live: release, health, instances |
| `releases` | Deploy history from cluster revisions |
| `logs` | Stream logs from all instances |
| `manifest` | Print the YAML buidl would apply |
| `config show/validate/environments` | Inspect resolved configuration |
| `cluster ...` | Install and manage the cluster behind an environment (see above) |

Global flags: `-e/--env`, `-f/--config`, `-o/--output {auto,pretty,plain,json}`, `-v/--verbose`, `--timeout`.

## CI

Output adapts automatically: plain and unfoldable in CI, colored on a terminal, newline-delimited JSON with `-o json`. Warnings and errors become native CI annotations so they surface in the run summary. Release ID, digest, and URL are exported as step outputs.

`buidl init` writes a workflow implementing the intended shape:

- **Pull request** → preview environment + a production `plan`, so a reviewer sees the running app and the infra delta.
- **Merge to main** → deploy to staging.
- **Manual dispatch** → `promote` staging's digest to production, gated on a GitHub environment approval.

Deploying a dirty working tree warns locally and **fails in CI**, where it signals a broken pipeline rather than a deliberate choice.

Exit codes: `0` success, `1` failure, `2` changes detected (`plan --detailed-exitcode`), `3` invalid configuration.

## Requirements

- **A BuildKit endpoint** for building (not needed with `driver: prebuilt`). Discovered from `build.addr`, `$BUILDKIT_HOST`, a rootless socket, or `/run/buildkit/buildkitd.sock`. If none is found, the error tells you how to get one.
- **Kubernetes 1.30+**, for server-side apply and the `preStop.sleep` lifecycle handler.
- **cert-manager** if you set `proxy.ssl: true`.
- **An ingress controller** if you set `proxy.host`.
- **metrics-server** if you use `deploy.autoscale`.

Registry credentials come from the standard Docker config, so `docker login`, `gcloud auth configure-docker`, and `docker/login-action` all work with no buidl-specific setup.

## What buidl creates

Per app and environment: `ServiceAccount`, `Deployment`, `Service`, and optionally `Secret`, `Ingress`, `HorizontalPodAutoscaler`, `PodDisruptionBudget`, `Namespace`.

All writes use server-side apply with the field manager `buidl`, so buidl owns exactly the fields it sets and will not clobber those set by an HPA, a mesh injector, or a human with kubectl. Objects carry the standard `app.kubernetes.io/*` labels plus `buidl.dev/*` provenance annotations (release, digest, commit, actor, timestamp).

## Status

The Kubernetes deploy backend and the k3s/RKE2 cluster installer are implemented end-to-end. The `deploy.Target` interface is backend-agnostic — plan, apply, wait for health, roll back — so an SSH/bare-metal backend can be added without reworking the command layer.

**Verified against a real cluster** (OrbStack Kubernetes 1.35 + ghcr.io): build, push, apply, rollout gating, health-check failure with auto-rollback, crash-loop detection, rollback, lifecycle hooks, pull secrets, and every inspection command. 53 acceptance assertions, all passing.

**Verified against real machines**: two bare Ubuntu VMs → a working two-node k3s cluster → a deployed app, in 5m44s, with pods scheduled across both nodes. Covers SSH auth, strict host-key verification, fact gathering, `cluster-init` with embedded etcd, worker join, node labels from inventory, kubeconfig fetch and merge, and convergence (a second run is a no-op and skips straight to the app deploy). See `examples/hello/buidl.vm.yaml`.

**Verified without a cluster:** configuration resolution, manifest rendering (round-tripped through the real Kubernetes typed scheme), node config rendering for every role and distribution, shell-injection safety, kubeconfig merge safety, inventory validation, project detection, the diff engine, field-level change extraction (including that secret values never leak into plan output), plan and deploy report rendering, step timing and summaries, and CI output modes.

**Not yet verified:** multi-control-plane HA (the one-at-a-time etcd join ordering has only been exercised with a single control plane), RKE2 (only k3s has been installed for real), version upgrades against a running cluster, `cluster reset`, blue-green cutover, accessories, multi-arch builds, and cert-manager/TLS issuance.

Not implemented: only `infra.provider: static` is wired up — the `inventory.Provider` interface exists so providers that read `tofu output -json`, an Ansible inventory, or an arbitrary script slot in without touching the cluster code, but today you paste addresses into `buidl.yaml`. Accessories are modeled in config but not reconciled (deliberately separate, so an app rollout can never restart your database). `deploy.target` values other than `kubernetes`.

Known limitation: a version `upgrade` re-runs the installer in place and restarts the unit, one control plane at a time. It does **not** cordon and drain nodes first, so pods are restarted rather than gracefully evicted. For a production upgrade, drain each node yourself or accept the restart.

## Development

```sh
make test        # unit tests — no cluster, no network
make lint
make build
make acceptance  # against a real cluster and registry
```

**Unit tests** cover configuration resolution, manifest rendering (round-tripped through the real Kubernetes typed scheme), release identity, project detection, Dockerfile invariants, layered secret resolution, hook execution, the diff engine, field-level change extraction, report rendering, and CI output modes. They need no cluster and no network.

**Acceptance tests** (`test/acceptance/run.sh`) cover what unit tests structurally cannot: BuildKit talking to a real registry, server-side apply, rollout gating, and the failure diagnostics. `examples/hello` is an app built to misbehave on demand — `FAIL_READINESS=1`, `CRASH_ON_BOOT=1`, `BOOT_DELAY=25s` — because the happy path is the easy part and the failure reporting is what needed proving.

```sh
docker run -d --name buildkitd --privileged moby/buildkit:latest
export BUILDKIT_HOST=docker-container://buildkitd
export DEMO_SECRET=any-value-for-testing
make acceptance                       # 53 assertions
KEEP=1 ./test/acceptance/run.sh healthy   # one case, keep the namespace
```

The cases assert on outcomes, not prose: that the deployed image is digest-pinned, that a crash loop is caught *before* the deploy timeout, that an auto-rollback restores the exact prior release, that a failing `pre-deploy` hook aborts before anything is applied, and that no command ever prints a secret value.

This suite earned its keep on its first run — it found six real bugs that the unit tests had passed cleanly, including two combinations of buidl's own generated output that could never have worked together.
