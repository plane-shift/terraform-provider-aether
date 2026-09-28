package provider_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// Expected coder symbol: NewClusterVolumesDataSource (datasource_cluster_volumes.go).
// T13-13: the object's inventory_available/last_synced_at/org-wide byte
// figures MUST be exposed too - a data source that only surfaces `volumes[]`
// turns "the platform could not look" into "you have no volumes" (spec
// OA:1349-1352's own warning, cited in the brief's defect #8).

func volumesDataSourceHCL(clusterID string) string {
	return dsProviderBlock + `data "aether_cluster_volumes" "v" {
  cluster_id = "` + clusterID + `"
}`
}

// S1. Fails today (RED) with "does not support data source
// \"aether_cluster_volumes\"". Once implemented, fails against a wrong
// implementation if inventory_available=false does not clear volumes to an
// empty list, if it is surfaced as an ERROR (it must not be - contract §5:
// "could not look" is always 200), or if inventory_available itself isn't
// exposed on the schema at all (the defect the brief's #8 names).
func TestDataSourceClusterVolumes_UnavailableIsEmptyListNoErrorFlagFalse(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.SeedVolumes(cid, 1000, 400, fakeapi.Volume{ID: "vol-1", Name: "data"})
	api.SetVolumesUnavailable(cid, true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: volumesDataSourceHCL(cid),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.#", "0"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "inventory_available", "false"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "last_synced_at", "0001-01-01T00:00:00Z"),
			),
		}},
	})
}

// S2. Fails if the data source does NOT sort by id itself (the fake
// preserves seed order verbatim, task13_routes_test.go's own
// TestVolumesRouteDoesNotSortItPreservesSeedOrder pins that on the fake side)
// - a coder who forwards the wire order unchanged would fail this test.
func TestDataSourceClusterVolumes_SortedByIDRegardlessOfFakeOrder(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.SeedVolumes(cid, 0, 0,
		fakeapi.Volume{ID: "vol-zzz", Name: "z"},
		fakeapi.Volume{ID: "vol-aaa", Name: "a"},
		fakeapi.Volume{ID: "vol-mmm", Name: "m"},
	)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: volumesDataSourceHCL(cid),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.#", "3"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.id", "vol-aaa"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.1.id", "vol-mmm"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.2.id", "vol-zzz"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "inventory_available", "true"),
			),
		}},
	})
}

// m5 (review round 1, GREEN today - fills a coverage gap, code unchanged):
// used_percent (a Float64Attribute) round-trips an exactly-representable
// seeded value without spurious floating-point formatting (e.g. "40" not
// "40.000000" or "39.99999999999999").
func TestDataSourceClusterVolumes_UsedPercentRoundTripsAnExactValue(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.SeedVolumes(cid, 0, 0, fakeapi.Volume{ID: "vol-1", Name: "data", UsedPercent: 40})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: volumesDataSourceHCL(cid),
			Check:  resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.used_percent", "40"),
		}},
	})
}

// Fails if a normal (available) read drops any of the 11 per-volume fields
// or the two org-wide byte figures.
func TestDataSourceClusterVolumes_AvailableReadsAllFields(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.SeedVolumes(cid, 5000, 1200,
		fakeapi.Volume{ID: "vol-1", Name: "shared-1", Type: "shared", StorageClass: "aether-shared",
			ProvisionedBytes: 0, UsedBytes: 300, UsedPercent: 0, BilledOn: "used",
			State: "Bound", Watchers: 2, CreatedAt: "2026-09-01T00:00:00Z"},
	)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: volumesDataSourceHCL(cid),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.id", "vol-1"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.name", "shared-1"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.type", "shared"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.storage_class", "aether-shared"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.provisioned_bytes", "0"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.used_bytes", "300"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.billed_on", "used"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.state", "Bound"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.watchers", "2"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "volumes.0.created_at", "2026-09-01T00:00:00Z"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "org_shared_quota_bytes", "5000"),
				resource.TestCheckResourceAttr("data.aether_cluster_volumes.v", "org_shared_used_bytes", "1200"),
			),
		}},
	})
}
