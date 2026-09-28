package provider_test

// Tests for the aether_registry data source (not yet implemented -
// datasource_registry.go, T14-3). Until NewRegistryDataSource is registered
// in provider.go's DataSources(), every test here fails at plan time with
// "Invalid Data Source Type" (see datasource_organization_members_test.go's
// header comment).

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const registryDataSource = "data.aether_registry.r"

const registryDSHCL = `
provider "aether" {}
data "aether_registry" "r" {}
`

// RG1. Fails if a disabled/never-enabled registry does not read enabled =
// false with every OTHER attribute null, or if it errors instead (T14-3: "a
// data source must be usable for conditional config").
func TestRegistryDataSource_DisabledIsFalseAndNullsNoError(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: registryDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(registryDataSource, "enabled", "false"),
				resource.TestCheckNoResourceAttr(registryDataSource, "id"),
				resource.TestCheckNoResourceAttr(registryDataSource, "harbor_project"),
				resource.TestCheckNoResourceAttr(registryDataSource, "endpoint"),
				resource.TestCheckNoResourceAttr(registryDataSource, "quota_gb"),
				resource.TestCheckNoResourceAttr(registryDataSource, "created_at"),
				resource.TestCheckNoResourceAttr(registryDataSource, "updated_at"),
			),
		}},
	})
}

// RG2. Fails if an enabled registry does not read every field (T14-3, no
// used_gb - dead field per WRONG #17).
func TestRegistryDataSource_EnabledReadsAllFields(t *testing.T) {
	api := setupFake(t)
	enabled := rawEnableRegistry(t, api, testOrg)
	regID := enabled["registry"].(map[string]any)["id"].(string)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: registryDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(registryDataSource, "enabled", "true"),
				resource.TestCheckResourceAttr(registryDataSource, "id", regID),
				resource.TestCheckResourceAttr(registryDataSource, "harbor_project", "aether-acme"),
				resource.TestCheckResourceAttr(registryDataSource, "quota_gb", "50"),
				resource.TestCheckResourceAttrSet(registryDataSource, "endpoint"),
				resource.TestCheckResourceAttrSet(registryDataSource, "created_at"),
				resource.TestCheckResourceAttrSet(registryDataSource, "updated_at"),
				checkAbsentFromStateSubstring(registryDataSource, "used_gb"),
			),
		}},
	})
}
