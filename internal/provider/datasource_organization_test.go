package provider_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// O1. Expected coder symbol: NewOrganizationDataSource (datasource_organization.go).
//
// Fails today (RED) with "does not support data source \"aether_organization\""
// - the data source is not registered. Once implemented, fails against a
// wrong implementation if: any of the 8 attributes is missing or wrong
// (including a null billing_email/vat_id when the fake never set them, T13-2),
// the read does not reuse flattenOrganization's absent-email/vat-id-is-null
// behaviour, or plan is anything but "starter".
func TestDataSourceOrganization_ReadsAllAttributes(t *testing.T) {
	api := setupFake(t)
	api.SetOrgBilling(testOrg, strp("billing@acme.test"), strp("RO123456"))
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: dsProviderBlock + `data "aether_organization" "o" {}`,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_organization.o", "id", testOrg),
				resource.TestCheckResourceAttr("data.aether_organization.o", "name", "Acme"),
				resource.TestCheckResourceAttr("data.aether_organization.o", "slug", "acme"),
				resource.TestCheckResourceAttr("data.aether_organization.o", "plan", "starter"),
				resource.TestCheckResourceAttr("data.aether_organization.o", "billing_email", "billing@acme.test"),
				resource.TestCheckResourceAttr("data.aether_organization.o", "vat_id", "RO123456"),
				resource.TestCheckResourceAttrSet("data.aether_organization.o", "created_at"),
				resource.TestCheckResourceAttrSet("data.aether_organization.o", "updated_at"),
			),
		}},
	})
}

// Fails if billing_email/vat_id read as "" instead of null when the fake
// never set them (optString semantics, same as the aether_organization
// resource).
func TestDataSourceOrganization_UnsetBillingFieldsAreNullNotEmpty(t *testing.T) {
	api := setupFake(t)
	_ = api
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: dsProviderBlock + `data "aether_organization" "o" {}`,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckNoResourceAttr("data.aether_organization.o", "billing_email"),
				resource.TestCheckNoResourceAttr("data.aether_organization.o", "vat_id"),
			),
		}},
	})
}
