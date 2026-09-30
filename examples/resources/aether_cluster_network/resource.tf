variable "cluster_id" {
  type = string
}

# Creating this OVERWRITES the cluster's current policy; destroying it OPENS the
# public endpoint to any source address. Entries must be canonical IPv4 prefixes.
resource "aether_cluster_network" "this" {
  cluster_id = var.cluster_id
  authorized_cidrs = [
    "203.0.113.0/24",
    "198.51.100.7/32",
  ]

  api_server_rate_limit = {
    max_connections_per_second = 50
    burst                      = 100
  }
}
