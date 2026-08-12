# Provisions a single Vultr server for buidl to install Kubernetes on.
#
# This is deliberately the whole division of labor: OpenTofu creates the machine,
# the network rules and the DNS-facing IP; buidl takes it from there. Nothing here
# knows about Kubernetes, and buidl never calls a cloud API.
#
#   tofu init
#   tofu apply
#   tofu output -raw ipv4          # put this in buidl.vultr.yaml and your DNS
#   tofu destroy                   # when finished — this costs money by the hour

terraform {
  required_version = ">= 1.6"

  required_providers {
    vultr = {
      source  = "vultr/vultr"
      version = "~> 2.23"
    }
    http = {
      source  = "hashicorp/http"
      version = "~> 3.4"
    }
  }
}

# The API key is read from VULTR_API_KEY rather than declared as a variable, so a
# credential can never end up in a .tfvars file or the state's variable block.
provider "vultr" {
  rate_limit  = 100
  retry_limit = 3
}

variable "hostname" {
  description = "Fully qualified hostname the app will be served on."
  type        = string
  default     = "hello.danecwalker.com"
}

variable "region" {
  description = "Vultr region id. syd = Sydney."
  type        = string
  default     = "syd"
}

variable "plan" {
  description = <<-EOT
    Vultr plan id. vc2-1c-2gb is ~$10/month.

    1GB is too small in practice: k3s plus Traefik plus cert-manager leaves almost
    nothing for the app, and the first thing to fail is the kubelet under memory
    pressure — which looks like a mysterious deploy timeout rather than an
    out-of-memory error.
  EOT
  type        = string
  default     = "vc2-1c-2gb"
}

variable "ssh_public_key_path" {
  description = "Public key authorized for root on the server."
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

variable "api_access_cidr" {
  description = <<-EOT
    CIDR allowed to reach the Kubernetes API on 6443.

    Defaults to this machine's current public address. The API server accepts
    cluster-admin credentials, so it should not be open to the internet even
    though it is TLS-protected.
  EOT
  type        = string
  default     = ""
}

# Look up the OS by name rather than hardcoding a numeric id, which Vultr changes
# between releases.
data "vultr_os" "ubuntu" {
  filter {
    name   = "name"
    values = ["Ubuntu 24.04 LTS x64"]
  }
}

# This machine's public address, used to lock down the Kubernetes API.
data "http" "my_ip" {
  url = "https://checkip.amazonaws.com"
}

locals {
  my_cidr = var.api_access_cidr != "" ? var.api_access_cidr : "${chomp(data.http.my_ip.response_body)}/32"
  # Vultr requires a label unique per account; the hostname is a natural one.
  label = replace(var.hostname, ".", "-")
}

resource "vultr_ssh_key" "deploy" {
  name    = "buidl-${local.label}"
  ssh_key = trimspace(file(pathexpand(var.ssh_public_key_path)))
}

resource "vultr_firewall_group" "cluster" {
  description = "buidl ${var.hostname}"
}

# SSH: buidl's only channel to the machine. Key authentication only — the Vultr
# image disables root password login by default when an SSH key is supplied.
resource "vultr_firewall_rule" "ssh" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "22"
  notes             = "ssh"
}

# HTTP must be open to the world, not just for serving: the ACME http-01
# challenge is fetched by Let's Encrypt from arbitrary addresses, so restricting
# port 80 would silently break certificate issuance.
resource "vultr_firewall_rule" "http" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "80"
  notes             = "http and acme http-01 challenge"
}

resource "vultr_firewall_rule" "https" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "443"
  notes             = "https"
}

# The Kubernetes API is restricted: it accepts cluster-admin credentials, and
# leaving it open to the internet is the most common way a small cluster is lost.
resource "vultr_firewall_rule" "kube_api" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = split("/", local.my_cidr)[0]
  subnet_size       = tonumber(split("/", local.my_cidr)[1])
  port              = "6443"
  notes             = "kubernetes api (restricted to the operator)"
}

resource "vultr_instance" "node" {
  region   = var.region
  plan     = var.plan
  os_id    = data.vultr_os.ubuntu.id
  label    = local.label
  hostname = split(".", var.hostname)[0]

  ssh_key_ids       = [vultr_ssh_key.deploy.id]
  firewall_group_id = vultr_firewall_group.cluster.id

  enable_ipv6      = true
  backups          = "disabled"
  activation_email = false

  # Nothing is installed here on purpose. Provisioning the machine and
  # configuring the cluster are separate jobs, and buidl owns the second one:
  # it installs k3s over SSH, so this stays a plain Ubuntu box.
  user_data = <<-EOT
    #cloud-config
    package_update: true
    packages:
      - curl
    write_files:
      - path: /etc/buidl-provisioned
        content: |
          Provisioned by OpenTofu for ${var.hostname}.
          Kubernetes is installed separately by `buidl deploy`.
  EOT
}

output "ipv4" {
  description = "Public address. Point the DNS A record here, and put it in buidl.vultr.yaml."
  value       = vultr_instance.node.main_ip
}

output "hostname" {
  description = "Hostname the app will be served on."
  value       = var.hostname
}

output "dns_record" {
  description = "The DNS record to create."
  value       = "${var.hostname}.  A  ${vultr_instance.node.main_ip}"
}

output "ssh" {
  description = "Command to reach the machine."
  value       = "ssh root@${vultr_instance.node.main_ip}"
}

output "api_access_cidr" {
  description = "CIDR permitted to reach the Kubernetes API."
  value       = local.my_cidr
}
