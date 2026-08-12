# Three-control-plane cluster, for exercising buidl's HA join ordering.
#
# The thing under test is buidl's sequencing, not Vultr: additional control
# planes must join ONE AT A TIME, because two etcd members joining concurrently
# can leave the cluster without a quorum. That code path has never run.
#
#   tofu init && tofu apply
#   tofu output -raw ips
#   tofu destroy          # these bill by the hour
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

provider "vultr" {
  rate_limit  = 100
  retry_limit = 3
}

variable "region" {
  type    = string
  default = "syd"
}

variable "plan" {
  description = "etcd is disk-latency sensitive, so this is not the place to economise."
  type        = string
  default     = "vc2-1c-2gb"
}

variable "count_control_planes" {
  description = <<-EOT
    Must be odd. etcd needs a strict majority to accept writes, so three members
    tolerate one failure while two tolerate none — worse than a single node.
  EOT
  type        = number
  default     = 3
}

variable "ssh_public_key_path" {
  type    = string
  default = "~/.ssh/id_ed25519.pub"
}

data "vultr_os" "ubuntu" {
  filter {
    name   = "name"
    values = ["Ubuntu 24.04 LTS x64"]
  }
}

data "http" "my_ip" {
  url = "https://checkip.amazonaws.com"
}

locals {
  my_cidr = "${chomp(data.http.my_ip.response_body)}/32"
}

resource "vultr_ssh_key" "deploy" {
  name    = "buidl-ha"
  ssh_key = trimspace(file(pathexpand(var.ssh_public_key_path)))
}

resource "vultr_firewall_group" "cluster" {
  description = "buidl ha test"
}

resource "vultr_firewall_rule" "ssh" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "22"
  notes             = "ssh"
}

# The API server, restricted to the operator.
resource "vultr_firewall_rule" "kube_api" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = split("/", local.my_cidr)[0]
  subnet_size       = tonumber(split("/", local.my_cidr)[1])
  port              = "6443"
  notes             = "kubernetes api"
}

# Nodes must reach each other for etcd, the API server and the overlay network.
# Without this the second control plane cannot join the first, which is precisely
# the path under test.
resource "vultr_firewall_rule" "intra_tcp" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "2379:2380"
  notes             = "etcd peers (scoped by the cluster token, not by network)"
}

resource "vultr_firewall_rule" "intra_api" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "6443"
  notes             = "api server for joining nodes"
}

resource "vultr_firewall_rule" "vxlan" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "udp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "8472"
  notes             = "flannel vxlan"
}

resource "vultr_firewall_rule" "kubelet" {
  firewall_group_id = vultr_firewall_group.cluster.id
  protocol          = "tcp"
  ip_type           = "v4"
  subnet            = "0.0.0.0"
  subnet_size       = 0
  port              = "10250"
  notes             = "kubelet"
}

resource "vultr_instance" "cp" {
  count = var.count_control_planes

  region   = var.region
  plan     = var.plan
  os_id    = data.vultr_os.ubuntu.id
  label    = "buidl-ha-cp${count.index + 1}"
  hostname = "cp${count.index + 1}"

  ssh_key_ids       = [vultr_ssh_key.deploy.id]
  firewall_group_id = vultr_firewall_group.cluster.id

  enable_ipv6      = false
  backups          = "disabled"
  activation_email = false

  # ufw ships active on this image allowing only SSH, which silently blocks the
  # API server, etcd and the overlay network. buidl detects and reports this but
  # deliberately does not change firewall rules, so provisioning handles it —
  # which is the correct division of responsibility.
  user_data = <<-EOT
    #cloud-config
    package_update: true
    runcmd:
      - ufw allow 6443/tcp
      - ufw allow 2379:2380/tcp
      - ufw allow 8472/udp
      - ufw allow 10250/tcp
      - ufw allow 80/tcp
      - ufw allow 443/tcp
      - ufw allow from 10.42.0.0/16 to any
      - ufw allow from 10.43.0.0/16 to any
      - ufw reload
  EOT
}

output "ips" {
  value = [for i in vultr_instance.cp : i.main_ip]
}

output "servers_yaml" {
  description = "Paste straight into infra.servers."
  value       = join("\n", [for i in vultr_instance.cp : "    - {host: ${i.main_ip}, role: control-plane}"])
}

output "endpoint" {
  description = "controlPlaneEndpoint. The first node's address here tests join ordering; a real HA setup needs a load balancer or DNS fronting all three."
  value       = vultr_instance.cp[0].main_ip
}
