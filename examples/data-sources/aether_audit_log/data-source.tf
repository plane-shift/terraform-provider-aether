# Returns ONE page: the newest `limit` events (1-100); older events are not paged.
data "aether_audit_log" "recent" {
  limit = 100
}

output "recent_actions" {
  value = [for e in data.aether_audit_log.recent.events : "${e.created_at} ${e.actor_email} ${e.action}"]
}
