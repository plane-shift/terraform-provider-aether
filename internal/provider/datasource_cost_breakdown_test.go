package provider_test

// Tests for the aether_cost_breakdown data source (not yet implemented -
// datasource_cost_breakdown.go, T14-10). Until NewCostBreakdownDataSource is
// registered in provider.go's DataSources(), every test here fails at plan
// time with "Invalid Data Source Type" (see
// datasource_organization_members_test.go's header comment).

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

const costBreakdownDataSource = "data.aether_cost_breakdown.b"

const costBreakdownDSHCL = `
provider "aether" {}
data "aether_cost_breakdown" "b" {}
`

// CB1. Fails if a per-pool autoscaling line and a STORAGE line (the point of
// T14-10 - node_pool_id null, pool_name holding a CLUSTER name, resource_type
// outside the vcpu/memory/disk enum) are not both rendered correctly, mixed
// in the SAME autoscaling list, or if subscriptions/subtotal_eur are wrong.
func TestCostBreakdownDataSource_ReadStorageAndPoolLines(t *testing.T) {
	api := setupFake(t)
	poolID := "77777777-7777-4777-8777-777777777777"
	api.SetBreakdown(testOrg, nil, []fakeapi.AutoscalingLine{
		{NodePoolID: &poolID, PoolName: "workers", ResourceType: "vcpu", Unit: "vcpu-hour", Quantity: 24, UnitPriceEUR: 0.01, AmountEUR: 0.24},
		{NodePoolID: nil, PoolName: "prod-cluster", ResourceType: "registry", Unit: "gib-hour", Quantity: 100, UnitPriceEUR: 0.001, AmountEUR: 0.1},
		{NodePoolID: nil, PoolName: "(deleted cluster)", ResourceType: "storage_rwx", Unit: "gib-hour", Quantity: 50, UnitPriceEUR: 0.002, AmountEUR: 0.1},
	}, 0.44)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: costBreakdownDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttrSet(costBreakdownDataSource, "period_start"),
				resource.TestCheckResourceAttrSet(costBreakdownDataSource, "period_end"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "subtotal_eur", "0.44"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "subscriptions.#", "0"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.#", "3"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.0.node_pool_id", poolID),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.0.pool_name", "workers"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.0.resource_type", "vcpu"),
				resource.TestCheckNoResourceAttr(costBreakdownDataSource, "autoscaling.1.node_pool_id"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.1.pool_name", "prod-cluster"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.1.resource_type", "registry"),
				resource.TestCheckNoResourceAttr(costBreakdownDataSource, "autoscaling.2.node_pool_id"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.2.pool_name", "(deleted cluster)"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.2.resource_type", "storage_rwx"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.2.unit", "gib-hour"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.2.quantity", "50"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.2.unit_price_eur", "0.002"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.2.amount_eur", "0.1"),
			),
		}},
	})
}

// Empty-month zeros, mirroring usage's own contract independently.
func TestCostBreakdownDataSource_EmptyMonthReadsZeros(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: costBreakdownDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(costBreakdownDataSource, "subtotal_eur", "0"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "subscriptions.#", "0"),
				resource.TestCheckResourceAttr(costBreakdownDataSource, "autoscaling.#", "0"),
			),
		}},
	})
}
