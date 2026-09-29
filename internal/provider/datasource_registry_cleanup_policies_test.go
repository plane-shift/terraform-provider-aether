package provider_test

// Tests for the aether_registry_cleanup_policies data source
// (datasource_registry_cleanup_policies.go, T14-7, registered as
// NewRegistryCleanupPoliciesDataSource in provider.go's DataSources()):
// condition value normalisation matching the resource's own, and
// last_run_message never leaking into state.

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const cleanupPoliciesDataSource = "data.aether_registry_cleanup_policies.p"

const cleanupPoliciesDSHCL = `
provider "aether" {}
data "aether_registry_cleanup_policies" "p" {}
`

// CP1. Fails if the data source does not normalise stored condition values
// to strings the same way aether_registry_cleanup_policy's resource does
// (a float64 10 -> "10", not "1e+01" or "10.0"; a null op/value stays null,
// not ""), or if last_run_message (never rendered by any list route,
// contract §10) leaks into state.
func TestRegistryCleanupPoliciesDataSource_ReadNormalisedConditions(t *testing.T) {
	api := setupFake(t)
	id := api.SeedPolicy(testOrg, "nightly-cleanup", "delete",
		[]map[string]any{
			{"type": "tag_count", "op": "gt", "value": float64(10)},
			{"type": "untagged"}, // no op, no value - stays null (§9)
		},
		[]string{"app/*"}, "nightly", true,
	)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: cleanupPoliciesDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.#", "1"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.id", id),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.name", "nightly-cleanup"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.action", "delete"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.schedule", "nightly"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.enabled", "true"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.repo_scope.#", "1"),
				resource.TestCheckTypeSetElemAttr(cleanupPoliciesDataSource, "policies.0.repo_scope.*", "app/*"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.conditions.#", "2"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.conditions.0.type", "tag_count"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.conditions.0.op", "gt"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.conditions.0.value", "10"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.conditions.1.type", "untagged"),
				resource.TestCheckNoResourceAttr(cleanupPoliciesDataSource, "policies.0.conditions.1.op"),
				resource.TestCheckNoResourceAttr(cleanupPoliciesDataSource, "policies.0.conditions.1.value"),
				resource.TestCheckNoResourceAttr(cleanupPoliciesDataSource, "policies.0.last_run_at"),
				resource.TestCheckNoResourceAttr(cleanupPoliciesDataSource, "policies.0.last_run_deleted"),
				resource.TestCheckNoResourceAttr(cleanupPoliciesDataSource, "policies.0.last_run_freed_bytes"),
				checkAbsentFromStateSubstring(cleanupPoliciesDataSource, "last_run_message"),
				resource.TestCheckResourceAttrSet(cleanupPoliciesDataSource, "policies.0.created_at"),
				resource.TestCheckResourceAttrSet(cleanupPoliciesDataSource, "policies.0.updated_at"),
			),
		}},
	})
}

// After a run, list-derived last_run_at/_deleted/_freed_bytes populate but
// last_run_message still never does (contract §10 - the list route omits it
// no matter what SetLastRun stored).
func TestRegistryCleanupPoliciesDataSource_AfterRunShowsLastRunFieldsNotMessage(t *testing.T) {
	api := setupFake(t)
	id := api.SeedPolicy(testOrg, "p", "delete", []map[string]any{{"type": "untagged", "value": "true"}}, nil, "manual", true)
	api.SetLastRun(testOrg, id, 4, 2048, "a message that must never appear in state")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: cleanupPoliciesDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.last_run_deleted", "4"),
				resource.TestCheckResourceAttr(cleanupPoliciesDataSource, "policies.0.last_run_freed_bytes", "2048"),
				resource.TestCheckResourceAttrSet(cleanupPoliciesDataSource, "policies.0.last_run_at"),
				checkAbsentFromStateSubstring(cleanupPoliciesDataSource, "last_run_message"),
			),
		}},
	})
}
