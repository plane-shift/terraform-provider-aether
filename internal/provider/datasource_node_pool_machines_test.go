package provider_test

import (
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// Expected coder symbol: NewNodePoolMachinesDataSource (datasource_node_pool_machines.go).

func machinesDataSourceHCL(clusterID, poolID string) string {
	return dsProviderBlock + `data "aether_node_pool_machines" "m" {
  cluster_id   = "` + clusterID + `"
  node_pool_id = "` + poolID + `"
}`
}

// M1. Fails today (RED) with "does not support data source
// \"aether_node_pool_machines\"". Once implemented, fails against a wrong
// implementation if any of the six fields is dropped/mistyped, or if the
// order the fake sorts by (name) isn't preserved through to state.
func TestDataSourceNodePoolMachines_AllFields(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	pid := seedPool(t, api, cid, "workers")
	api.SeedMachines(pid,
		fakeapi.Machine{Name: "worker-1", Status: "Ready", K8sVersion: "v1.37.2", InternalIP: "10.64.1.1", AgeSeconds: 7200, CreatedAt: "2026-09-20T00:00:00Z"},
		fakeapi.Machine{Name: "worker-2", Status: "NotReady", K8sVersion: "v1.37.2", InternalIP: "10.64.1.2", AgeSeconds: 60, CreatedAt: "2026-09-27T23:00:00Z"},
	)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: machinesDataSourceHCL(cid, pid),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.#", "2"),
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.0.name", "worker-1"),
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.0.status", "Ready"),
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.0.k8s_version", "v1.37.2"),
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.0.internal_ip", "10.64.1.1"),
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.0.age_seconds", "7200"),
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.0.created_at", "2026-09-20T00:00:00Z"),
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.1.name", "worker-2"),
				resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.1.status", "NotReady"),
			),
		}},
	})
}

// M2. Fails if the "no CAPI name / no kube client" 200 [] is surfaced as an
// error instead of an empty list.
func TestDataSourceNodePoolMachines_UnavailableIsEmptyNotAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	pid := seedPool(t, api, cid, "workers")
	api.SeedMachines(pid, fakeapi.Machine{Name: "worker-1", Status: "Ready"})
	api.SetMachinesUnavailable(pid, true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: machinesDataSourceHCL(cid, pid),
			Check:  resource.TestCheckResourceAttr("data.aether_node_pool_machines.m", "machines.#", "0"),
		}},
	})
}

// M3. Fails if a 502 kube_list_failed is swallowed instead of surfacing as an
// error.
func TestDataSourceNodePoolMachines_KubeListFailedIsAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	pid := seedPool(t, api, cid, "workers")
	api.SetMachinesListFails(pid, true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      machinesDataSourceHCL(cid, pid),
			ExpectError: loose("failed to list machines"),
		}},
	})
}

// m4 (review round 1): an upper-case UUID on node_pool_id must be rejected
// with the canonical-UUID wording, with zero API requests. Same measured
// GREEN-today status as the cluster/node_pool m4 tests (see their doc
// comments): parseInputID already runs before any request inside Read.
func TestDataSourceNodePoolMachines_UpperCaseNodePoolIDIsRejectedWithZeroRequests(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	before := len(requestsMatching(api, http.MethodGet, `/machines$`))
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      machinesDataSourceHCL(cid, upperCaseUUID),
			ExpectError: loose("canonical"),
		}},
	})
	if reqs := requestsMatching(api, http.MethodGet, `/machines$`); len(reqs) != before {
		t.Fatalf("an invalid node_pool_id reached the API with a machines GET: %v", reqs)
	}
}

// A 404 on an unknown node pool id is an error, not an empty list.
//
// n2 (review round 1): asserts on "404" (the status), not on the fake's
// "not found" message text (loose("not found") tolerates a wrong error
// path too, and cannot be phrased against the "not_found" error CODE at
// all - \W+ never matches an underscore).
func TestDataSourceNodePoolMachines_UnknownPoolIsAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      machinesDataSourceHCL(cid, zeroUUID),
			ExpectError: loose("404"),
		}},
	})
	if reqs := requestsMatching(api, http.MethodGet, `/machines$`); len(reqs) == 0 {
		t.Fatalf("expected at least one machines GET to have been attempted (and 404'd)")
	}
}
