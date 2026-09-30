# Repositories of the organization's registry, read live from the registry.
data "aether_registry_repositories" "all" {}

output "repositories" {
  value = { for r in data.aether_registry_repositories.all.repositories : r.name => r.artifact_count }
}
