package provider_test

// Tests for the aether_registry_robot_accounts data source (not yet
// implemented - datasource_registry_robot_accounts.go, T14-6). Until
// NewRegistryRobotAccountsDataSource is registered in provider.go's
// DataSources(), every test here fails at plan time with "Invalid Data
// Source Type" (see datasource_organization_members_test.go's header
// comment).

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const robotAccountsDataSource = "data.aether_registry_robot_accounts.r"

const robotAccountsDSHCL = `
provider "aether" {}
data "aether_registry_robot_accounts" "r" {}
`

// RB1. Fails if id/name/permissions/harbor_id/harbor_username/created_at/
// updated_at is wrong, if permissions is not exposed as a Set, or if
// harbor_username is anything but the RAW value the list route returns
// (CG's RobotAccount type drops it entirely - contract §6, so a correct
// implementation MUST raw-decode). Also fails if `secret` or `last_used_at`
// (never written, contract §6) leak into state.
func TestRegistryRobotAccountsDataSource_ReadRawHarborUsername(t *testing.T) {
	api := setupFake(t)
	id := api.SeedRobot(testOrg, "ci-pusher", []string{"push", "pull"})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: robotAccountsDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(robotAccountsDataSource, "robot_accounts.#", "1"),
				resource.TestCheckResourceAttr(robotAccountsDataSource, "robot_accounts.0.id", id),
				resource.TestCheckResourceAttr(robotAccountsDataSource, "robot_accounts.0.name", "ci-pusher"),
				resource.TestCheckResourceAttr(robotAccountsDataSource, "robot_accounts.0.permissions.#", "2"),
				resource.TestCheckTypeSetElemAttr(robotAccountsDataSource, "robot_accounts.0.permissions.*", "push"),
				resource.TestCheckTypeSetElemAttr(robotAccountsDataSource, "robot_accounts.0.permissions.*", "pull"),
				resource.TestCheckResourceAttr(robotAccountsDataSource, "robot_accounts.0.harbor_username", "robot$aether-acme+ci-pusher"),
				resource.TestCheckResourceAttrSet(robotAccountsDataSource, "robot_accounts.0.harbor_id"),
				resource.TestCheckResourceAttrSet(robotAccountsDataSource, "robot_accounts.0.created_at"),
				resource.TestCheckResourceAttrSet(robotAccountsDataSource, "robot_accounts.0.updated_at"),
				resource.TestCheckNoResourceAttr(robotAccountsDataSource, "robot_accounts.0.secret"),
				resource.TestCheckNoResourceAttr(robotAccountsDataSource, "robot_accounts.0.last_used_at"),
			),
		}},
	})
}
