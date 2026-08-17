---
title: Clusters
group: Guides
description: buidl never creates VMs. Once the machines exist, list them and deploy.
---

buidl never creates VMs, networks, firewalls, or load balancers. Once the machines exist, list them:

```sh
buidl add server 203.0.113.10 --email you@example.com
buidl add server 203.0.113.11 --role worker
buidl deploy --dry-run
buidl deploy
```

There is no separate "create the cluster" command. `deploy --dry-run` inspects the servers and includes any Kubernetes install they need. If the cluster is already there, it also fetches its kubeconfig so the application diff can run. `deploy` converges the cluster, then ships the app.

Use `deploy --skip-cluster` against a cluster you manage yourself.

## What the file looks like

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
    certManagerEmail: ops@acme.com
  servers:
    - {host: 203.0.113.1, role: control-plane, privateIP: 10.0.0.1}
    - {host: 203.0.113.10, role: worker, labels: {pool: web}}
```

Only `infra.provider: static` is implemented. Addresses go in `buidl.yaml`.

## cluster commands

Hidden from default help. For inspection and teardown:

| Command | What it does |
|---|---|
| `cluster inventory` | resolved fleet, and which machine bootstraps |
| `cluster status` | node health, including machines that never joined |
| `cluster kubeconfig` | fetch credentials and merge into `~/.kube/config` |
| `cluster reset` | uninstall Kubernetes. Leaves the servers running. Type the environment name, or pass `--yes`. |

## Dual-stack

New clusters are dual-stack. Pod and service IPv6 is ULA (`fd00::/8`), so it does not consume the host's public `/64`. This is required for public TLS: Let's Encrypt prefers IPv6 whenever a name has an AAAA. An IPv4-only ingress on a host with a public AAAA fails HTTP-01 and Traefik keeps serving its default certificate.

DNS and the cloud firewall have to match:

- A record: the server's IPv4
- AAAA record: the server's IPv6 (on Hetzner, `2a01:...::1` from the assigned `/64`, not the `/64` itself)
- 80/443 open from `0.0.0.0/0` and `::/0`

`privateIP` still pins `node-ip` to the private IPv4. If the host also has a global IPv6, buidl appends it so ServiceLB publishes both families.

Opt out by setting both CIDRs to IPv4-only. An existing IPv4-only cluster cannot pick this up in place. Rewriting `cluster-cidr` on a running node crash-loops flannel. Rebuild it:

```sh
buidl cluster reset -e production
buidl deploy -e production
```

## Behaviour

- The first control-plane server initializes the cluster. Extra control planes join one at a time. Workers join in parallel.
- Re-runs converge. A healthy node whose rendered config matches is left alone (`up-to-date`).
- If no server can be reached, that is an error, not "no changes".
- Two control planes is worse than one. buidl warns. More than one control plane without `controlPlaneEndpoint` is an error.
- Host keys are checked by default. Unknown keys fail with instructions to run `ssh-keyscan`, or set `infra.ssh.acceptNewHostKeys: true`. A mismatched key always fails.
- After converging, deploys use the kubeconfig context buidl created (`<app>-<environment>`), not whatever happens to be current.
- A version `upgrade` re-runs the installer in place and restarts the unit, one control plane at a time. It does not cordon or drain first.

OpenTofu owns the machines. `examples/hello/infra` creates a Vultr VM, firewall, and SSH key. buidl never calls a cloud API.
