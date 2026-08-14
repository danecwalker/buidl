# Security

## Reporting a vulnerability

Report privately via GitHub's [private vulnerability
reporting](https://github.com/danecwalker/buidl/security/advisories/new) rather
than opening a public issue.

Please include what an attacker gains, the configuration required, and a way to
reproduce it. A working proof of concept is welcome but not required.

## What buidl is trusted with

This matters more than usual, because buidl runs privileged operations on
infrastructure you own. It holds or handles:

- **SSH access as root** to every server in `infra.servers`, used to install
  system packages, write systemd units and start services.
- **Cluster-admin credentials**, fetched from a control-plane node and merged
  into your local kubeconfig.
- **Registry credentials**, read from the standard Docker config to push images
  and, with `registry.createPullSecret`, copied into the cluster as a pull
  secret.
- **Application secrets**, resolved at deploy time and written into Kubernetes
  Secrets.

A vulnerability in any of those paths is worth reporting even if exploiting it
requires an unusual configuration.

## Deliberate security decisions

These are choices, not oversights. If you think one is wrong, that is worth
raising.

**SSH host keys are verified by default.** Cluster bootstrap installs
root-level software and copies a cluster-admin credential back, so an
unverified connection is a machine-in-the-middle window at the worst possible
moment. `infra.ssh.acceptNewHostKeys` opts into trust-on-first-use for hosts not
yet in `known_hosts`; a *mismatched* key remains a hard failure regardless.

**buidl never changes host firewall rules.** It detects an active firewall and
prints the exact ports the cluster needs, but applies nothing. A mistaken rule
can sever SSH and lock you out of a machine, and a printed command that is
slightly wrong costs far less than an applied one that is.

**Secret values are never printed.** Plan output routinely ends up in a pull
request or a CI log. Secret changes are reported by key name only, and
`variable list` reports presence and length. There are tests that fail if a value
reaches any output path.

**Shell arguments are escaped.** Every value interpolated into a remote command
is single-quote escaped, and file contents are streamed over stdin rather than
interpolated into a command. The test suite round-trips hostile inputs through a
real shell.

**`.buidl/secrets-common` is committed on purpose.** It contains only
indirections (`DATABASE_URL=$PROD_DATABASE_URL`), never values. Committing it is
what makes the set of required secrets reviewable. The files that may hold
literal values — `.buidl/secrets` and `.buidl/secrets.<environment>` — are
gitignored, and buidl warns when their permissions are too open.

**Generated images run as a non-root numeric UID.** Kubernetes cannot verify a
*named* user against `runAsNonRoot` and will refuse to start the container, so
every generated Dockerfile declares a numeric UID.

**The in-cluster BuildKit addon is rootless.** A privileged buildkitd is
effectively root on the node, and a build runs the least trustworthy code in the
system.

## Supported versions

While the major version is 0, only the latest release receives fixes.
