# Changelog

Notable changes per release. Dates are the tag date.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the major version is 0, the configuration schema may change between minor
versions. Breaking schema changes are always listed first and always say what to
do about them.

## [Unreleased]

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

[Unreleased]: https://github.com/danecwalker/buidl/compare/v0.1.7...HEAD
[0.1.7]: https://github.com/danecwalker/buidl/releases/tag/v0.1.7
[0.1.6]: https://github.com/danecwalker/buidl/releases/tag/v0.1.6
[0.1.5]: https://github.com/danecwalker/buidl/releases/tag/v0.1.5
[0.1.4]: https://github.com/danecwalker/buidl/releases/tag/v0.1.4
[0.1.3]: https://github.com/danecwalker/buidl/releases/tag/v0.1.3
[0.1.2]: https://github.com/danecwalker/buidl/releases/tag/v0.1.2
[0.1.1]: https://github.com/danecwalker/buidl/releases/tag/v0.1.1
[0.1.0]: https://github.com/danecwalker/buidl/releases/tag/v0.1.0
