# Changelog

Notable changes per release. Dates are the tag date.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
versions follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the major version is 0, the configuration schema may change between minor
versions. Breaking schema changes are always listed first and always say what to
do about them.

## [Unreleased]

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

- **`accessories` is modeled in the configuration schema but not implemented.**
  It parses and validates, and then nothing is deployed.
- No published release binaries yet, so the documented `curl` install does not
  work. Build from source.
- RKE2, blue-green cutover, version upgrades against a running cluster and
  multi-arch builds are implemented but have never been exercised against real
  infrastructure.
- A version `upgrade` re-runs the installer in place and restarts the unit one
  control plane at a time. It does **not** cordon and drain first, so pods are
  restarted rather than gracefully evicted.

[Unreleased]: https://github.com/danewalker/buidl/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/danewalker/buidl/releases/tag/v0.1.0
