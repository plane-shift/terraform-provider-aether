# The address must already have an Aether login (portal invitation flow).
# Changing role REPLACES the membership; the last owner cannot be removed.
resource "aether_organization_member" "ops" {
  email = "ops@example.com"
  role  = "admin"
}
