variable "cluster_id" {
  type = string
}

# Admin kubeconfig (sensitive, stored in state); readable only while the cluster is running.
data "aether_kubeconfig" "this" {
  cluster_id = var.cluster_id
}

output "kubeconfig" {
  value     = data.aether_kubeconfig.this.content
  sensitive = true
}
