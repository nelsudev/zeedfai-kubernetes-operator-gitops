# zeedfai — k3s cluster on Hetzner Cloud
#
# Hourly billing: bring it up for the demo, run the burst, then
# `terraform destroy`. Every server gets the zeedfai=true label so the
# nightly teardown-cloud-demo.yml workflow can delete orphaned servers.

terraform {
  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.49"
    }
  }
}

variable "hcloud_token" {
  description = "Hetzner Cloud API token (project → Security → API tokens)"
  type        = string
  sensitive   = true
}

variable "ssh_public_key" {
  description = "SSH public key for node access"
  type        = string
}

variable "worker_count" {
  description = "Number of fixed workers; cluster-autoscaler adds elastic ones"
  type        = number
  default     = 1
}

variable "server_type" {
  description = "Hetzner server type for the control-plane and fixed workers"
  type        = string
  default     = "ccx13"
}

variable "location" {
  description = "Hetzner location where servers are created"
  type        = string
  default     = "fsn1"
}

provider "hcloud" {
  token = var.hcloud_token
}

locals {
  control_plane_private_ip = "10.0.1.10"
  worker_private_ips       = [for i in range(var.worker_count) : cidrhost("10.0.1.0/24", 20 + i)]
}

resource "hcloud_ssh_key" "zeedfai" {
  name       = "zeedfai"
  public_key = var.ssh_public_key
}

resource "hcloud_network" "zeedfai" {
  name     = "zeedfai"
  ip_range = "10.0.0.0/16"
}

resource "hcloud_network_subnet" "nodes" {
  network_id   = hcloud_network.zeedfai.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = "10.0.1.0/24"
}

resource "hcloud_server" "control_plane" {
  name        = "zeedfai-cp"
  server_type = var.server_type
  image       = "ubuntu-24.04"
  location    = var.location
  ssh_keys    = [hcloud_ssh_key.zeedfai.id]
  labels      = { zeedfai = "true", role = "control-plane" }

  network {
    network_id = hcloud_network.zeedfai.id
    ip         = local.control_plane_private_ip
  }

  user_data = templatefile("${path.module}/cloud-init-cp.yaml", {
    k3s_token  = random_password.k3s_token.result
    private_ip = local.control_plane_private_ip
  })

  depends_on = [hcloud_network_subnet.nodes]
}

resource "hcloud_server" "worker" {
  count       = var.worker_count
  name        = "zeedfai-worker-${count.index}"
  server_type = var.server_type
  image       = "ubuntu-24.04"
  location    = var.location
  ssh_keys    = [hcloud_ssh_key.zeedfai.id]
  labels      = { zeedfai = "true", role = "worker" }

  network {
    network_id = hcloud_network.zeedfai.id
    ip         = local.worker_private_ips[count.index]
  }

  user_data = templatefile("${path.module}/cloud-init-worker.yaml", {
    k3s_token  = random_password.k3s_token.result
    cp_ip      = local.control_plane_private_ip
    private_ip = local.worker_private_ips[count.index]
  })

  depends_on = [hcloud_server.control_plane]
}

resource "random_password" "k3s_token" {
  length  = 48
  special = false
}

output "control_plane_ip" {
  value = hcloud_server.control_plane.ipv4_address
}

output "next_steps" {
  value = <<-EOT
    1. ssh root@${hcloud_server.control_plane.ipv4_address}
    2. copy /etc/rancher/k3s/k3s.yaml and replace 127.0.0.1 with the public IP
    3. bootstrap Flux against the Git branch/path you want to validate
    4. install the Hetzner cluster-autoscaler provider if node scaling is in scope
    5. run the burst demo; when finished, run terraform destroy
  EOT
}
