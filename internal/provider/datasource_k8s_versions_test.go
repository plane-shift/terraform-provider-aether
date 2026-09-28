package provider_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// V1. Expected coder symbol: NewK8sVersionsDataSource (datasource_k8s_versions.go).
//
// Fails today (RED) with "does not support data source \"aether_k8s_versions\"".
// Once implemented, fails against a wrong implementation if `latest` is
// anything but the FIRST item's patch (not the item labelled Latest looked up
// by label - the two must be the same item, but a coder computing `latest`
// with a label-filter instead of index-0 would still pass THIS assertion; the
// V2 test below is what catches that class), if the "items" key is read as
// "versions", or if the list is reordered.
func TestDataSourceK8sVersions_LatestIsTheFirstItemsPatch(t *testing.T) {
	api := setupFake(t)
	api.SetK8sVersions(
		fakeapi.K8sVersion{Version: "1.38", Label: "Latest", Patch: "1.38.1"},
		fakeapi.K8sVersion{Version: "1.37", Label: "Stable", Patch: "1.37.9"},
		fakeapi.K8sVersion{Version: "1.36", Label: "Stable", Patch: "1.36.20"},
	)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: dsProviderBlock + `data "aether_k8s_versions" "v" {}`,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.#", "3"),
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.0.version", "1.38"),
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.0.label", "Latest"),
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.0.patch", "1.38.1"),
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.1.version", "1.37"),
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.1.label", "Stable"),
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.1.patch", "1.37.9"),
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.2.patch", "1.36.20"),
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "latest", "1.38.1"),
			),
		}},
	})
}

// V2. Fails if an empty window (SetK8sVersions with zero items) surfaces as
// an ERROR instead of an empty list (contract §2: 200 {"items":[]}, never
// 503), or if `latest` reads as "" instead of null when there is no first
// item to take it from.
func TestDataSourceK8sVersions_EmptyListMeansVersionsEmptyAndLatestNull(t *testing.T) {
	api := setupFake(t)
	api.SetK8sVersions()
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: dsProviderBlock + `data "aether_k8s_versions" "v" {}`,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_k8s_versions.v", "versions.#", "0"),
				resource.TestCheckNoResourceAttr("data.aether_k8s_versions.v", "latest"),
			),
		}},
	})
}
