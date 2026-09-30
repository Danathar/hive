# Dedicated CI cluster for hivecommons self-hosted GitHub Actions runners.
# See ../README.md for the runbook; this file only creates the Linode side.

resource "linode_lke_cluster" "hive_ci" {
  label       = var.cluster_label
  k8s_version = var.k8s_version
  region      = var.region
  tags        = var.tags

  # Standard tier (LKE Enterprise is not available on this account). HA control
  # plane is what ARC needs: the listener holds a long-poll to GitHub and a
  # single-replica API server restart drops every in-flight job assignment.
  control_plane {
    high_availability = true

    dynamic "acl" {
      for_each = length(var.control_plane_acl_cidrs) > 0 ? [1] : []
      content {
        enabled = true
        addresses {
          ipv4 = var.control_plane_acl_cidrs
        }
      }
    }
  }

  # ARC controller, listener, registry pull-through cache. Tainted after
  # creation (README step 2) so runner pods never land here.
  pool {
    type  = var.system_pool_type
    count = var.system_pool_count
    tags  = ["arc-system"]
    labels = {
      "hive-role" = "system"
    }
    taint {
      key    = "hive-role"
      value  = "system"
      effect = "NoSchedule"
    }
  }

  # Runner pods only. Local NVMe (655 GB on g8-dedicated-64-32) carries the
  # per-node module/tool caches; no shared RWX volume anywhere.
  pool {
    type  = var.runner_pool_type
    count = var.runner_pool_count
    tags  = ["runners"]
    labels = {
      "hive-ci-runner" = "true"
    }
    autoscaler {
      min = var.runner_pool_min
      max = var.runner_pool_max
    }
  }
}

locals {
  node_instance_ids = flatten([
    for p in linode_lke_cluster.hive_ci.pool : [for n in p.nodes : n.instance_id]
  ])
}

# Inbound: nothing but the LKE control plane (kubelet 10250, Calico/Wireguard)
# and node-to-node traffic. Outbound: DNS, HTTP(S) only — GitHub, GHCR,
# proxy.golang.org, apt mirrors. Attach to every cluster Linode.
resource "linode_firewall" "hive_ci_nodes" {
  label           = "${var.cluster_label}-nodes"
  tags            = var.tags
  inbound_policy  = "DROP"
  outbound_policy = "DROP"

  inbound {
    label    = "kubelet-from-control-plane"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "10250"
    ipv4     = ["192.168.128.0/17"]
  }
  inbound {
    label    = "node-to-node-tcp"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "1-65535"
    ipv4     = ["192.168.128.0/17"]
  }
  inbound {
    label    = "node-to-node-udp"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "1-65535"
    ipv4     = ["192.168.128.0/17"]
  }
  inbound {
    label    = "calico-wireguard"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "51820"
    ipv4     = ["192.168.128.0/17"]
  }

  outbound {
    label    = "dns"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "53"
    ipv4     = ["0.0.0.0/0"]
  }
  outbound {
    label    = "http-https"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "80,443"
    ipv4     = ["0.0.0.0/0"]
    ipv6     = ["::/0"]
  }
  outbound {
    label    = "cluster-internal"
    action   = "ACCEPT"
    protocol = "TCP"
    ports    = "1-65535"
    ipv4     = ["192.168.128.0/17", "10.0.0.0/8"]
  }
  outbound {
    label    = "cluster-internal-udp"
    action   = "ACCEPT"
    protocol = "UDP"
    ports    = "1-65535"
    ipv4     = ["192.168.128.0/17", "10.0.0.0/8"]
  }

  linodes = local.node_instance_ids
}
