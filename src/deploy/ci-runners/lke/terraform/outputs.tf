output "cluster_id" {
  value = linode_lke_cluster.hive_ci.id
}

output "api_endpoints" {
  value = linode_lke_cluster.hive_ci.api_endpoints
}

output "kubeconfig" {
  value     = linode_lke_cluster.hive_ci.kubeconfig
  sensitive = true
}

output "pool_ids" {
  value = { for p in linode_lke_cluster.hive_ci.pool : p.tags[0] => p.id }
}
