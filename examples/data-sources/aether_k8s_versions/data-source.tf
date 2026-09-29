# Offered Kubernetes versions; latest is null when the version window is unavailable.
data "aether_k8s_versions" "offered" {}

output "latest" {
  value = data.aether_k8s_versions.offered.latest
}

output "all" {
  value = [for v in data.aether_k8s_versions.offered.versions : v.version]
}
