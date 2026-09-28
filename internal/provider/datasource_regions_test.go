package provider_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// R1. The plan's reference implementation is verbatim (T13-3), so this test
// pins the reference file's own contract rather than inventing a new one.
//
// Fails today (RED) with "does not support data source \"aether_regions\"".
// Once implemented, fails against a wrong implementation if the fake's one
// eu-fi region is missing, wrong, or if `available` reads anything but true.
func TestDataSourceRegions_ReadsTheFakesOneRegion(t *testing.T) {
	api := setupFake(t)
	_ = api
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: dsProviderBlock + `data "aether_regions" "r" {}`,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_regions.r", "regions.#", "1"),
				resource.TestCheckResourceAttr("data.aether_regions.r", "regions.0.id", "eu-fi"),
				resource.TestCheckResourceAttr("data.aether_regions.r", "regions.0.name", "Europe (Finland)"),
				resource.TestCheckResourceAttr("data.aether_regions.r", "regions.0.location", "Helsinki"),
				resource.TestCheckResourceAttr("data.aether_regions.r", "regions.0.available", "true"),
			),
		}},
	})
}

// m1 (review round 1, RED until the coder lands ruling T13-17): an empty
// result must round-trip as an EMPTY list, never a null one -
// `datasource_regions.go`'s `var out regionsModel` leaves Regions nil today,
// so this fails against the CURRENT code with a "expected []any value for
// ListSizeExact check, got: <nil>" mismatch, not just against a hypothetical
// wrong implementation. Once T13-17 lands (`out := regionsModel{Regions:
// []regionModel{}}`), both checks below pass.
func TestDataSourceRegions_EmptyResultIsAnEmptyListNotNull(t *testing.T) {
	api := setupFake(t)
	api.SetRegionsEmpty(true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: dsProviderBlock + `data "aether_regions" "r" {}`,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue("data.aether_regions.r", tfjsonpath.New("regions"), knownvalue.ListSizeExact(0)),
			},
			Check: resource.TestCheckResourceAttr("data.aether_regions.r", "regions.#", "0"),
		}},
	})
}
