package provider_test

// Tests for the aether_usage data source (not yet implemented -
// datasource_usage.go, T14-9). Until NewUsageDataSource is registered in
// provider.go's DataSources(), every test here fails at plan time with
// "Invalid Data Source Type" (see datasource_organization_members_test.go's
// header comment).

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

const usageDataSource = "data.aether_usage.u"

const usageDSHCL = `
provider "aether" {}
data "aether_usage" "u" {}
`

// US1. Fails if period_start/period_end/total_eur/subscriptions is missing
// or wrong, and - the point of T14-9/WRONG #12 - if an "items" (or
// "autoscaling") attribute exists AT ALL in the schema: the unbounded,
// unaggregated hourly usage_records list must not be mirrored into state.
func TestUsageDataSource_ReadAllAttributesNoItemsKey(t *testing.T) {
	api := setupFake(t)
	clusterID := "66666666-6666-4666-8666-666666666666"
	api.SetUsage(testOrg,
		[]map[string]any{{"id": "u1", "resource_type": "registry"}, {"id": "u2", "resource_type": "vcpu"}},
		[]fakeapi.SubscriptionLine{{Kind: "plan", ClusterID: &clusterID, Name: "starter", AmountEUR: 20, GrossAmountEUR: 20, DiscountEUR: 0}},
		20.75,
	)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: usageDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttrSet(usageDataSource, "period_start"),
				resource.TestCheckResourceAttrSet(usageDataSource, "period_end"),
				resource.TestCheckResourceAttr(usageDataSource, "total_eur", "20.75"),
				resource.TestCheckResourceAttr(usageDataSource, "subscriptions.#", "1"),
				resource.TestCheckResourceAttr(usageDataSource, "subscriptions.0.kind", "plan"),
				resource.TestCheckResourceAttr(usageDataSource, "subscriptions.0.cluster_id", clusterID),
				resource.TestCheckResourceAttr(usageDataSource, "subscriptions.0.name", "starter"),
				resource.TestCheckResourceAttr(usageDataSource, "subscriptions.0.amount_eur", "20"),
				resource.TestCheckResourceAttr(usageDataSource, "subscriptions.0.gross_amount_eur", "20"),
				resource.TestCheckResourceAttr(usageDataSource, "subscriptions.0.discount_eur", "0"),
				resource.TestCheckNoResourceAttr(usageDataSource, "subscriptions.0.node_pool_id"),
				// The whole point of T14-9: no "items" attribute exists on the
				// data source's flat state AT ALL, not even an empty one.
				func(s *terraform.State) error {
					for k := range attrs(s, usageDataSource) {
						if k == "items" || k == "items.#" || hasPrefixDS(k, "items.") || k == "autoscaling" || k == "autoscaling.#" {
							return errAttrPresent(usageDataSource, k)
						}
					}
					return nil
				},
			),
		}},
	})
}

func hasPrefixDS(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func errAttrPresent(addr, key string) error {
	return &attrPresentError{addr: addr, key: key}
}

type attrPresentError struct{ addr, key string }

func (e *attrPresentError) Error() string {
	return e.addr + " has attribute " + e.key + ", want it absent (T14-9 drops usage.items)"
}

// Fails if a genuinely empty (never-Set) org does not read back zeros - a
// data source read has no way to distinguish "empty" from "error" other than
// this.
func TestUsageDataSource_EmptyMonthReadsZeros(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: usageDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(usageDataSource, "total_eur", "0"),
				resource.TestCheckResourceAttr(usageDataSource, "subscriptions.#", "0"),
			),
		}},
	})
}
