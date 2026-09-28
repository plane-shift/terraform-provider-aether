package provider_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// Expected coder symbol: NewLoadBalancersDataSource (datasource_loadbalancers.go).
// T13-12: the provider MUST decode res.Body itself - ListLoadBalancersWithResponse's
// JSON200 silently yields zero-valued items against the real shape (§4). A
// coder who used the typed JSON200 would make this test's `ip`/`purpose`
// assertions fail on empty strings instead of the seeded values.

func loadBalancersDataSourceHCL(clusterID string) string {
	return dsProviderBlock + `data "aether_loadbalancers" "lb" {
  cluster_id = "` + clusterID + `"
}`
}

// L1. Fails today (RED) with "does not support data source
// \"aether_loadbalancers\"". Once implemented, fails against a wrong
// implementation if the stale OA/CG fields (service_name/namespace/
// external_ip/dns_name/ports) leak into the schema instead of the real
// {ip,purpose,service_ref,allocated_at}, or if a cp_lb item's null
// service_ref surfaces as "" instead of null.
func TestDataSourceLoadBalancers_DecodesTheRealShape(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	uid := "tenant-svc-uid-42"
	allocated := "2026-09-15T00:00:00Z"
	api.SeedLoadBalancers(cid,
		fakeapi.LoadBalancer{IP: "203.0.113.10", Purpose: "cp_lb", AllocatedAt: &allocated},
		fakeapi.LoadBalancer{IP: "203.0.113.20", Purpose: "ccm_lb", ServiceRef: &uid, AllocatedAt: &allocated},
	)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: loadBalancersDataSourceHCL(cid),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.#", "2"),
				resource.TestCheckResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.0.ip", "203.0.113.10"),
				resource.TestCheckResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.0.purpose", "cp_lb"),
				resource.TestCheckNoResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.0.service_ref"),
				resource.TestCheckResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.1.ip", "203.0.113.20"),
				resource.TestCheckResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.1.purpose", "ccm_lb"),
				resource.TestCheckResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.1.service_ref", uid),
				resource.TestCheckResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.1.allocated_at", allocated),
			),
		}},
	})
}

// m5 (review round 1) / n1 SUMMARY (review round 2, deviation 2, DEFERRED at
// r2 time): a 200 whose body is valid JSON but carries no "items" key at all
// must be an error, not a silently-empty list. The shared nonJSONSummary
// ("Aether API answered 200 without a JSON body") is inaccurate for this
// branch - the body IS JSON, it just has no "items" list, and the detail
// already says so - so this is pinned on the accurate summary round 2 asks
// for instead: wording that names both "JSON" and "no \"items\"" without
// claiming there is no JSON body at all (e.g. "Aether API answered 200 with
// JSON that has no \"items\" list"). RED against current HEAD, since the
// datasource still reports nonJSONSummary for this branch (measured: the
// looser "no items list" fragment alone is already present in today's
// DETAIL text and would pass vacuously, which is why the pinned fragment
// also requires "with JSON" immediately before it - current wording says
// "with A JSON BODY that has no", which does not match). Goes GREEN once the
// coder's one-line summary change lands.
func TestDataSourceLoadBalancers_BodyWithoutItemsKeyIsAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.SetLoadBalancersRawBody(cid, `{"foo":"bar"}`)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      loadBalancersDataSourceHCL(cid),
			ExpectError: loose(`with JSON that has no "items"`),
		}},
	})
}

// Fails if an unseeded cluster's loadbalancers is anything but an empty list
// (never an error - the API returns 200 {"items":[]} for "no addresses
// allocated").
func TestDataSourceLoadBalancers_UnseededIsAnEmptyList(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: loadBalancersDataSourceHCL(cid),
			Check:  resource.TestCheckResourceAttr("data.aether_loadbalancers.lb", "loadbalancers.#", "0"),
		}},
	})
}
