package provider_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Expected coder symbol: NewClusterNetworkDataSource (datasource_cluster_network.go).
// flattenClusterNetwork is ALREADY a free function (resource_cluster_network.go:446)
// - no refactor needed here, unlike cluster/node-pool (T13-14).

func networkDataSourceHCL(clusterID string) string {
	return dsProviderBlock + `data "aether_cluster_network" "n" {
  cluster_id = "` + clusterID + `"
}`
}

// putRawNetwork PUTs …/network directly against the fake, bypassing the
// aether_cluster_network resource entirely (this data source must work
// standalone, without that resource ever having been applied).
func putRawNetwork(t *testing.T, apiURL, token, clusterID, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, apiURL+"/api/v1/organizations/"+testOrg+"/clusters/"+clusterID+"/network", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seeding PUT network = %d, want 200", resp.StatusCode)
	}
}

// W1. Fails today (RED) with "does not support data source
// \"aether_cluster_network\"". Once implemented, fails against a wrong
// implementation if authorized_cidrs/api_server_rate_limit/updated_at don't
// round-trip through flattenClusterNetwork exactly as the resource's own
// tests already pin (resource_cluster_network_test.go).
func TestDataSourceClusterNetwork_ReadsConfiguredPolicy(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	putRawNetwork(t, api.URL(), api.Token(), cid,
		`{"authorized_cidrs":["10.0.0.0/24","192.168.1.1/32"],"api_server_rate_limit":{"max_connections_per_second":50,"burst":100}}`)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: networkDataSourceHCL(cid),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_cluster_network.n", "cluster_id", cid),
				resource.TestCheckResourceAttr("data.aether_cluster_network.n", "authorized_cidrs.#", "2"),
				resource.TestCheckResourceAttr("data.aether_cluster_network.n", "authorized_cidrs.0", "10.0.0.0/24"),
				resource.TestCheckResourceAttr("data.aether_cluster_network.n", "authorized_cidrs.1", "192.168.1.1/32"),
				resource.TestCheckResourceAttr("data.aether_cluster_network.n", "api_server_rate_limit.max_connections_per_second", "50"),
				resource.TestCheckResourceAttr("data.aether_cluster_network.n", "api_server_rate_limit.burst", "100"),
				resource.TestCheckResourceAttrSet("data.aether_cluster_network.n", "updated_at"),
			),
		}},
	})
}

// Fails if a never-configured cluster's synthetic default (empty list, null
// rate limit, the Go zero time) isn't reproduced exactly - the same default
// the resource's own import test pins.
func TestDataSourceClusterNetwork_NeverConfiguredReadsTheSyntheticDefault(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: networkDataSourceHCL(cid),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_cluster_network.n", "authorized_cidrs.#", "0"),
				resource.TestCheckNoResourceAttr("data.aether_cluster_network.n", "api_server_rate_limit.%"),
				resource.TestCheckResourceAttr("data.aether_cluster_network.n", "updated_at", "0001-01-01T00:00:00Z"),
			),
		}},
	})
}
