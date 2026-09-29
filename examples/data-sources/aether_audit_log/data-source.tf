# Returns ONE page: the newest `limit` events (1-100); older events are not paged.
data "aether_audit_log" "recent" {
  limit = 100
}

output "recent_actions" {
  # actor_email is null for api_token and system actors; coalesce so the
  # interpolation below never fails on exactly those events.
  value = [for e in data.aether_audit_log.recent.events : "${e.created_at} ${coalesce(e.actor_email, e.actor_type)} ${e.action}"]
}
