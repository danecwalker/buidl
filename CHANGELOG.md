# Changelog

Notable changes per release. Dates are the tag date.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the major version is 0, the configuration schema may change between minor
versions. Breaking schema changes are always listed first and always say what to
do about them.

## [Unreleased]

## [0.5.1] — 2026-08-17

### Fixed

- `buidl watch` no longer smears tables across the screen. In a live
  session a newline did not return the cursor to column zero, so each
  refresh continued to the right and leftover characters from the
  loading frame stayed put. Long app and pod names are truncated so
  they cannot wrap.

## [0.5.0] — 2026-08-17

### Added

- `buidl watch` is a live terminal dashboard of the stack: health, ready
  counts, RAM, CPU, uptime, restarts, and cluster nodes. `j`/`k` select
  an app's instances. RAM and CPU come from metrics-server (k3s bundles
  it); without it those columns show `—`. `--once` and non-TTY stdout
  print one snapshot. The default 30m command timeout does not apply
  unless `--timeout` is set.
- `buidl init` without `--registry` writes `image: buidl.local/<app>`.
  `deploy` builds a local archive, copies it onto every `infra.servers`
  node, imports it into containerd, deploys with `imagePullPolicy:
  Never`, and deletes the temporary tars. A kubeconfig-only cluster
  without SSH cannot take this path. Legacy `ghcr.io/change-me/*`
  images still sideload.

## [0.4.1] — 2026-08-17

### Fixed

- `buidl init` no longer leaves the "Detecting project" spinner running
  over the setup questions. Detection was already instant; the spinner
  made it look hung, and its redraw could erase the y/n prompt.

## [0.4.0] — 2026-08-16

### Changed

- Omitted `deploy.kubernetes.createNamespace` defaults to true, so
  `buidl init` then `buidl deploy` creates the app namespace. Set
  `createNamespace: false` to manage the namespace yourself. `init`
  writes the setting so the generated file shows the default.
- `buidl init` is a setup wizard. On a terminal it asks whether you want
  GitHub Actions, then staging, then review apps, and writes the workflow
  and overlays. `--ci`, `--staging`, `--preview`, and `--no-ci` answer the
  same questions without a prompt. `add domain` fills template staging /
  production / preview hosts from the hostname you give it.
- The CLI is a stack of apps. Default help is `init`, `add`, `deploy`,
  `status`, `logs`, `rollback`, `destroy`, `update`. `add postgres` /
  `add redis` / `add api` are the same verb. `deploy --dry-run` replaces
  `plan`. `status --history` replaces `releases`. `deploy postgres`
  reconciles a stateful app. Advanced commands stay implemented and are
  hidden. Existing files need no migration.
- `add NAME` writes an extra process app under `apps:` (optional; files
  without that key are unchanged). `add domain --app` attaches a hostname
  to that process. A stack `deploy` rolls out every process app and
  creates missing stateful apps. Object names for extras use the app
  key; they share the stack namespace.

## [0.3.0] — 2026-08-15

### Changed

- The default product is one live app. `buidl init` no longer writes
  `defaultEnvironment: staging` or staging / production / preview overlays.
  `buidl deploy` with no `-e` targets that single app. Existing files that
  declare `environments` keep working, including implied staging.
- `buidl init` no longer writes a GitHub Actions workflow. Pass `--ci` for a
  single job that deploys on push to `main`. `--no-ci` is now a no-op alias.
- Omitted `deploy.strategy.type` defaults to `bluegreen`. Files that set
  `type: rolling` are unchanged. Init-generated files from ≤0.2.3 set rolling
  explicitly, so they do not flip.
- `buidl add` is noun-first: `add server`, `add domain`, `add postgres`,
  `add redis`, `add app`. `--database` and `--service` remain as hidden aliases.
- The first `buidl environment new` sets `defaultEnvironment` to that name
  (not only when the name is staging).
- `buidl destroy` requires `-e` only when overlays are declared. A
  single-target file can destroy without `-e` (still `--yes` in CI).

### Added

- `buidl add server HOST` writes `infra.servers` (and k3s / SSH defaults).
  It does not create a VM. `--email` sets `certManagerEmail`.
- `buidl add domain HOST` writes `proxy.host` and `proxy.ssl`. A second
  domain (`api.example.com`, `www`, …) is appended to `proxy.hosts` on the
  same app: one Ingress, one certificate.

## [0.2.3] — 2026-08-15

### Changed

- `install.sh` writes the binary to `~/.local/bin` so `buidl update` does
  not need sudo. If that directory is not on PATH, the script links
  `/usr/local/bin/buidl` at it (sudo once). An existing root-owned
  install is moved on the next `buidl update` (even when the version is
  already current) and prints a one-time `sudo ln -s` hint. Run `buidl
  update` without sudo; wrapping it in sudo keeps the binary in a
  root-owned directory. Set `BUIDL_INSTALL_DIR` to pick another location.

## [0.2.2] — 2026-08-14

### Changed

- `registry.createPullSecret` now defaults to true when omitted and
  `registry.pullSecret` is unset, so `buidl init` then `buidl deploy` can
  pull a private GHCR image without extra YAML. The same credential used
  to push is copied into the cluster as an imagePullSecret. Set
  `createPullSecret: false` to keep credentials off the cluster (a public
  image, or a node-level registry config). `buidl init` writes the
  setting so the generated file shows the default. If the default is on
  and no local credential exists, the secret is skipped so a public
  image and `buidl manifest` still work.

## [0.2.1] — 2026-08-14

### Changed

- `buidl plan` fetches kubeconfig for a managed cluster that already exists,
  the same way `deploy` does. A first plan no longer dies on a missing
  local context after it has just inspected the fleet over SSH.
  `status`, `logs`, `releases`, `rollback`, `destroy`, and `accessory`
  do the same fetch when credentials are missing.

### Fixed

- Deploy and `variable list` resolve accessory secrets (such as
  `POSTGRES_PASSWORD` on `type: postgres`) from `.buidl/secrets`. Previously
  only names listed under the app's `env.secret` were loaded, so a typed
  Postgres accessory failed preflight even when the password was on disk.
  Accessory-only secrets are still not injected into the app.

## [0.2.0] — 2026-08-14

### Changed

- **Breaking:** HTTP apps default to Kubernetes z-pages instead of `/up`.
  Readiness probes `GET /readyz`, liveness `GET /livez`, startup `GET /startupz`.
  Existing apps that only implement `/up` should add the three endpoints, or
  set `deploy.healthcheck.path: /up` to keep one path for all three probes
  (Rails/Kamal). Set `healthcheck.readiness` / `liveness` / `startup` to
  override a single probe. A timed-out deploy names those paths; buidl does
  not fall back to another endpoint.

### Added

- HTTP probes use the container's named `http` port, so a port change cannot
  leave probes pointed at a stale number.
- Postgres and Redis accessories get `pg_isready` / `redis-cli ping` startup,
  readiness, and liveness probes. `accessory plan` reports an exec-probe
  change as a restart.

## [0.1.8] — 2026-08-14

### Changed

- `install.sh` installs to `/usr/local/bin` and prompts for sudo when that
  directory is not writable, instead of falling back to `~/.local/bin`.
  Set `BUIDL_INSTALL_DIR` to pick another location.

## [0.1.7] — 2026-08-14

### Added

- `buidl update` replaces this binary with the latest GitHub release after
  verifying `checksums.txt`. Other commands print a notice when a newer
  release exists and point at `buidl update`. The check is off in CI and
  when `BUIDL_NO_UPDATE_CHECK=1`.
- `install.sh` is the one-line installer:
  `curl -fsSL https://raw.githubusercontent.com/danecwalker/buidl/main/install.sh | bash`

## [0.1.6] — 2026-08-14

### Changed

- **Breaking:** `buidl env` now manages environments (alias of `environment`).
  Variables moved to `buidl variable` (`var`, `vars`). `env list` previously
  printed release variables and never printed secret values; that command is
  now `variable list`.

### Added

- `buidl environment list|new|set|delete` writes environment overlays into
  `buidl.yaml` and keeps the comments `init` wrote. Templates match the
  staging / production / preview shapes from `init`. `--from` copies an
  existing overlay. Deleting the default environment requires `--force`.
- `buidl add --database postgres|redis` writes a typed accessory
  (`type: postgres`) and generates secrets. `add --service --host` configures
  this app. A second named service is an error: one app per file today.
- `buidl variable set` / `delete` write `.buidl/secrets` and declare the name
  under `env.secret`. `--clear` writes a non-secret into the file.
- Accessories accept `type: postgres` or `type: redis`. Omitted image, port,
  storage, mount path, and `POSTGRES_PASSWORD` are filled at load. A declared
  `DATABASE_URL` / `REDIS_URL` with no value is derived from the accessory.

## [0.1.5] — 2026-08-14

### Changed

- The happy path is `buidl init` then `buidl deploy`. Staging is implied when
  `-e` is omitted and an environment named `staging` exists (or
  `defaultEnvironment` is set). Production is never implied. `buidl destroy`
  always requires `-e`.
- An HTTP app that omits both `replicas` and `autoscale` gets a
  HorizontalPodAutoscaler (CPU 70%). Bounds come from the fleet
  (`infra.servers`), then from Ready schedulable nodes, then a 1–4 fallback.
  Preview environments stay at one replica. Workers with only an exec
  healthcheck stay at one replica. Existing configs that set `replicas` stay
  static.
- `proxy.ssl` plus `infra.addons.certManagerEmail` turns cert-manager on.
  `deploy.autoscale` turns metrics-server on unless k3s already bundles it.
- `buidl deploy` creates accessories that are not in the cluster. It never
  updates existing ones. `buidl accessory apply` is still the reconcile path.
- `buidl init` writes `defaultEnvironment: staging`, omits static replica
  counts, includes a commented `infra` block, and tells you to run
  `buidl deploy`.

## [0.1.4] — 2026-08-14

### Added

- **`buidl destroy`** tears down an environment. Preview namespaces (slug-derived,
  `createNamespace: true`, or `deploy.kubernetes.ephemeral: true`) are deleted
  wholesale. Staging and production lose only the app objects; accessories and
  the namespace stay. Production also requires `--force`. `--dry-run` prints the
  plan. `--stale 7d` sweeps leaked preview namespaces.
- The workflow `buidl init` writes now listens for `pull_request` `closed` and
  runs `buidl destroy -e preview --yes`, so a merge (or a close without merge)
  does not leave the preview namespace behind.

## [0.1.3] — 2026-08-14

### Changed

- New clusters are dual-stack. Defaults are `10.42.0.0/16,fd00:42::/56` (pods)
  and `10.43.0.0/16,fd00:43::/112` (services). k3s also gets
  `flannel-ipv6-masq`. `privateIP` still pins IPv4; a host global IPv6 is
  appended to `node-ip` so ServiceLB publishes both families.
- An existing IPv4-only cluster cannot take this in place (flannel crash-loops).
  Run `buidl cluster reset` then `buidl deploy`. Set both CIDRs to IPv4-only to
  opt out. Let's Encrypt prefers IPv6 when an AAAA exists, so IPv4-only ingress
  cannot complete HTTP-01.

## [0.1.2] — 2026-08-14

### Changed

- Ctrl+C asks for confirmation: a second press within 5s exits the process. The
  first press no longer cancels the command. SIGTERM still exits immediately.
- BuildKit discovery finds a Docker/Podman/nerdctl container named `buildkitd`
  (and will start it if it is stopped), or any running `moby/buildkit`
  container such as a Buildx builder. If none exists, buidl creates
  `moby/buildkit:v0.25.1` (pinned to the client in go.mod, not `:latest`).
  `BUILDKIT_HOST` is no longer required.

## [0.1.1] — 2026-08-13

### Changed

- The Go module path is now `github.com/danecwalker/buidl`, matching the
  published repository. `go install github.com/danecwalker/buidl/cmd/buidl@latest`
  works from this tag.
- The workflow `buidl init` writes downloads the binary from
  `github.com/danecwalker/buidl`. The previous `danewalker` URL 404s.
- README rewritten to match the CLI and examples.

## [0.1.0] — 2026-08-13

First tagged release.

### Added

- **Builds without a Docker daemon.** BuildKit is driven over its gRPC API and
  the image is exported straight to the registry, so there is no local image
  store and no separate `docker push` to fail. Supports `unix://`, `tcp://`,
  `docker-container://`, `kube-pod://`, `podman-container://`,
  `nerdctl-container://` and `ssh://` builder addresses.
- **Immutable, digest-pinned releases.** Every deploy mints a release ID and
  pins the image by digest, so a pod restart cannot pick up different bytes.
- **`promote`** deploys the exact digest running in one environment to another
  with no rebuild, carrying the source release's git provenance.
- **Cluster installation.** `plan` and `deploy` install k3s or RKE2 on bare
  servers over SSH and join them into a cluster. There is no separate bootstrap
  command: a fresh fleet and an established cluster take the same command.
  Additional control planes join one at a time, because concurrent etcd joins
  can cost the cluster its quorum.
- **Layered secrets** across `.env`, `.buidl/secrets-common` (committed,
  indirections only), `.buidl/secrets` and `.buidl/secrets.<environment>`, with
  the process environment always winning so CI injection is never overridden.
- **Lifecycle hooks** (`pre-build`, `post-build`, `pre-deploy`, `post-deploy`,
  `deploy-failed`) receiving the release identity and every resolved secret,
  which is what makes a `pre-deploy` migration possible without giving the
  application a schema-altering credential.
- **Reporting** designed around four questions: what will happen, what did
  happen, what failed, and what is running. Field-level plan diffs with runtime
  effect, per-step timing, partial-failure accounting, and newline-delimited
  JSON where every line decodes into one event type.
- Addons: cert-manager with a Let's Encrypt ClusterIssuer, metrics-server, and
  a rootless in-cluster BuildKit.
- Host firewall detection. buidl reports the exact ports a cluster needs but
  never changes firewall rules, because a mistaken rule can sever SSH.

### Known limitations

- **Accessories have never been applied to a real cluster.** They render, plan
  and apply, but only unit tests have exercised them.
- Accessories are never pruned, are fixed at one replica, and their rollout is
  not waited on.
- RKE2, blue-green cutover, version upgrades against a running cluster and
  multi-arch builds are implemented but have never been exercised against real
  infrastructure.
- A version `upgrade` re-runs the installer in place and restarts the unit one
  control plane at a time. It does **not** cordon and drain first, so pods are
  restarted rather than gracefully evicted.

[Unreleased]: https://github.com/danecwalker/buidl/compare/v0.5.1...HEAD
[0.5.1]: https://github.com/danecwalker/buidl/releases/tag/v0.5.1
[0.5.0]: https://github.com/danecwalker/buidl/releases/tag/v0.5.0
[0.4.1]: https://github.com/danecwalker/buidl/releases/tag/v0.4.1
[0.4.0]: https://github.com/danecwalker/buidl/releases/tag/v0.4.0
[0.3.0]: https://github.com/danecwalker/buidl/releases/tag/v0.3.0
[0.2.3]: https://github.com/danecwalker/buidl/releases/tag/v0.2.3
[0.2.2]: https://github.com/danecwalker/buidl/releases/tag/v0.2.2
[0.2.1]: https://github.com/danecwalker/buidl/releases/tag/v0.2.1
[0.2.0]: https://github.com/danecwalker/buidl/releases/tag/v0.2.0
[0.1.8]: https://github.com/danecwalker/buidl/releases/tag/v0.1.8
[0.1.7]: https://github.com/danecwalker/buidl/releases/tag/v0.1.7
[0.1.6]: https://github.com/danecwalker/buidl/releases/tag/v0.1.6
[0.1.5]: https://github.com/danecwalker/buidl/releases/tag/v0.1.5
[0.1.4]: https://github.com/danecwalker/buidl/releases/tag/v0.1.4
[0.1.3]: https://github.com/danecwalker/buidl/releases/tag/v0.1.3
[0.1.2]: https://github.com/danecwalker/buidl/releases/tag/v0.1.2
[0.1.1]: https://github.com/danecwalker/buidl/releases/tag/v0.1.1
[0.1.0]: https://github.com/danecwalker/buidl/releases/tag/v0.1.0
