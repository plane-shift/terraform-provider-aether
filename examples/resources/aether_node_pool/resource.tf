variable "cluster_id" {
  type = string
}

# Fixed pool: billed as a flat monthly fee; apply waits until desired_nodes nodes are ready.
# vcpus, memory_gb, disk_gb and name REPLACE the pool when changed.
resource "aether_node_pool" "workers" {
  cluster_id            = var.cluster_id
  name                  = "workers"
  vcpus                 = 4
  memory_gb             = 16
  disk_gb               = 80
  desired_nodes         = 3
  drain_timeout_seconds = 600 # once set it cannot be removed, only changed

  node_labels = {
    "team" = "platform"
  }
}

# Autoscaling pool: max_vcpus above min_vcpus (or max_memory_gb above min_memory_gb) makes it
# autoscaling, billed hourly per node; the autoscaler picks the node count inside the budget.
resource "aether_node_pool" "batch" {
  cluster_id    = var.cluster_id
  name          = "batch"
  vcpus         = 8
  memory_gb     = 32
  disk_gb       = 100
  min_vcpus     = 8
  max_vcpus     = 64
  min_memory_gb = 32
  max_memory_gb = 256

  scale_down_delay_seconds       = 900
  scale_down_utilization_percent = 40

  node_taints = [{
    key    = "workload"
    value  = "batch"
    effect = "NoSchedule" # a NoSchedule taint needs another untainted pool in the cluster
  }]
}
