# Import-only: this resource cannot create an organization (see the import command below).
# The slug cannot be changed from Terraform; "" clears an optional field.
resource "aether_organization" "this" {
  name          = "Example Ltd"
  billing_email = "billing@example.com"
  vat_id        = "FI12345678"
}
