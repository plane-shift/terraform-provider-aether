package provider_test

// Tests for the aether_registry_robot_accounts data source
// (datasource_registry_robot_accounts.go, T14-6, registered as
// NewRegistryRobotAccountsDataSource in provider.go's DataSources()): the
// raw-decoded harbor_username, permissions exposed as a Set, and secret/
// last_used_at never leaking into state.

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
