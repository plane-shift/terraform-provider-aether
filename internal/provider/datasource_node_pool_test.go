package provider_test

import (
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Expected coder symbols: NewNodePoolDataSource (datasource_node_pool.go),
// NewNodePoolsDataSource (datasource_node_pools.go).

// Basic read by id. Fails today (RED) with "does not support data source
// \"aether_node_pool\"". Once implemented, fails against a wrong
// implementation if any attribute is dropped by the extracted flatten
// (resource_node_pool.go:785, a method today per T13-14).
func TestDataSourceNodePool_ByIDReadsCoreAttributes(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	pid := seedPool(t, api, cid, "workers")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: nodePoolDataSourceHCL(cid, pid, ""),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "id", pid),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "cluster_id", cid),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "name", "workers"),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "vcpus", "2"),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "memory_gb", "4"),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "disk_gb", "20"),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "desired_nodes", "1"),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "min_nodes", "1"),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "max_nodes", "1"),
				resource.TestCheckResourceAttrSet("data.aether_node_pool.p", "created_at"),
				resource.TestCheckResourceAttrSet("data.aether_node_pool.p", "updated_at"),
			),
		}},
	})
}

// N3: a pool that is still `deleting` can still be matched by name (the list
// route never filters it out - it's live until the reconciler sweep stamps
// deleted_at). Fails if a `deleting` pool is silently excluded from the
// by-name match, or if its status isn't reported as "deleting".
func TestDataSourceNodePool_ByNameMatchesADeletingPool(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	pid := seedPool(t, api, cid, "workers")
	resp, _ := http.NewRequest(http.MethodDelete, api.URL()+"/api/v1/organizations/"+testOrg+"/clusters/"+cid+"/nodepools/"+pid, nil)
	resp.Header.Set("Authorization", "Bearer "+api.Token())
	if r, err := http.DefaultClient.Do(resp); err != nil {
		t.Fatalf("seeding DELETE: %v", err)
	} else if r.StatusCode != http.StatusAccepted {
		t.Fatalf("seeding DELETE = %d, want 202", r.StatusCode)
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: nodePoolDataSourceHCL(cid, "", "workers"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "id", pid),
				resource.TestCheckResourceAttr("data.aether_node_pool.p", "status", "deleting"),
			),
		}},
	})
}

// A 404 by id is an error.
func TestDataSourceNodePool_ByIDNotFoundIsAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      nodePoolDataSourceHCL(cid, zeroUUID, ""),
			ExpectError: loose("not found"),
		}},
	})
}

// Zero matches by name is an error.
func TestDataSourceNodePool_ByNameZeroMatchesIsAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      nodePoolDataSourceHCL(cid, "", "no-such-pool"),
			ExpectError: loose("no node pool named"),
		}},
	})
}

// Neither id nor name is a config error, zero API calls.
func TestDataSourceNodePool_NeitherIDNorNameIsAConfigError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      nodePoolDataSourceHCL(cid, "", ""),
			ExpectError: exactlyOneOfErrorRE,
		}},
	})
	if reqs := requestsMatching(api, http.MethodGet, poolsRE); len(reqs) != 0 {
		t.Fatalf("a config-time validation failure reached the API: %v", reqs)
	}
}

// P2: aether_node_pools walks two pages (a full page then a partial one -
// no trailing empty page, since 150 is not an exact multiple of 100).
// Fails if item count != 150, or if the second GET doesn't carry the first
// page's last id as its cursor.
func TestDataSourceNodePools_WalksTwoPages(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	ids := api.SeedManyNodePools(cid, 150)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: dsProviderBlock + `data "aether_node_pools" "all" {
  cluster_id = "` + cid + `"
}`,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_node_pools.all", "node_pools.#", "150"),
				resource.TestCheckResourceAttr("data.aether_node_pools.all", "node_pools.0.id", ids[0]),
				resource.TestCheckResourceAttr("data.aether_node_pools.all", "node_pools.149.id", ids[149]),
			),
		}},
	})
	reqs := requestsMatching(api, http.MethodGet, poolsRE)
	if len(reqs) == 0 || len(reqs)%2 != 0 {
		t.Fatalf("got %d GET .../nodepools requests, want a positive multiple of 2 (one [100][50] walk per Read invocation)", len(reqs))
	}
	last2 := reqs[len(reqs)-2:]
	if !containsQueryParam(last2[0].Query, "limit=100") {
		t.Fatalf("first request does not carry limit=100: %q", last2[0].Query)
	}
	if containsQueryParam(last2[0].Query, "cursor=") {
		t.Fatalf("first request carries a cursor, want none: %q", last2[0].Query)
	}
	if !containsQueryParam(last2[1].Query, "cursor="+ids[99]) {
		t.Fatalf("second request cursor = %q, want cursor=%s", last2[1].Query, ids[99])
	}
}
