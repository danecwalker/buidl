---
title: Install
group: Start
description: linux and darwin, amd64 and arm64. The binaries are static.
---

```sh
curl -fsSL https://raw.githubusercontent.com/danecwalker/buidl/main/install.sh | bash
```

That detects the platform, shows download progress, and verifies the SHA-256 against `checksums.txt` from the same GitHub release. The binary lands in `~/.local/bin` so later `buidl update` does not need sudo. If that directory is not on PATH, the installer links `/usr/local/bin/buidl` at it (sudo once). Set `BUIDL_INSTALL_DIR` to install somewhere else.

Later upgrades are `buidl update`. Run it without sudo.

linux and darwin, amd64 and arm64. The binaries are static (`CGO_ENABLED=0`) and built with `-trimpath`.

## go install

With Go 1.25+:

```sh
go install github.com/danecwalker/buidl/cmd/buidl@latest
```

`go install` does not stamp the version, so the binary reports `dev`. Use the installer or `task install` if you need `buidl --version` to match a tag. `buidl update` will replace a `dev` binary with the latest release.

## From source

```sh
git clone https://github.com/danecwalker/buidl.git
cd buidl
task build      # ./bin/buidl
task install    # $GOPATH/bin/buidl, version stamped from git describe
```

`buidl --version` reports the release tag on published binaries and the git describe string on a `task install` build.

## Requirements

- A BuildKit endpoint, unless `build.driver: prebuilt`. Discovery order: `build.addr`, `$BUILDKIT_HOST`, a rootless socket, `/run/buildkit/buildkitd.sock`, then a Docker/Podman/nerdctl container named `buildkitd` (or any running `moby/buildkit`, including a Buildx builder). If none of those exist, buidl creates `moby/buildkit:v0.25.1` as `buildkitd`.
- Kubernetes 1.30+, for server-side apply and `preStop.sleep`.
- cert-manager if `proxy.ssl: true`. Set `infra.addons.certManagerEmail` and buidl installs it.
- An ingress controller if `proxy.host` is set.
- metrics-server if `deploy.autoscale` is set. buidl installs it on RKE2, or when k3s's bundled copy has been disabled.
- For public TLS with an AAAA record: 80 and 443 reachable on IPv6. Let's Encrypt validates over IPv6 first.
