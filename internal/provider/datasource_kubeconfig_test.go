package provider_test

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// Expected coder symbol: NewKubeconfigDataSource (datasource_kubeconfig.go).
// T13-11: implementation MUST use the raw d.pd.Client.GetKubeconfig(ctx, org,
// id) + io.ReadAll - GetKubeconfigWithResponse fails on every 200 (it
// YAML-unmarshals a mapping into a Go string). A coder who used the
// WithResponse variant would make every test below fail with a decode error
// instead of the assertions here, which is exactly the class this task
// exists to prevent.

func kubeconfigDataSourceHCL(clusterID string) string {
	return dsProviderBlock + `data "aether_kubeconfig" "k" {
  cluster_id = "` + clusterID + `"
}`
}

const seededKubeconfigYAML = "apiVersion: v1\nkind: Config\nclusters:\n- name: seeded\n  cluster:\n    server: https://seeded.example.test:6443\ncontexts: []\ncurrent-context: \"\"\nusers: []\n"

// K1. Fails today (RED) with "does not support data source
// \"aether_kubeconfig\"". Once implemented, fails against a wrong
// implementation if `content` differs from the fake's YAML by even one byte
// (e.g. a coder who trims trailing whitespace, or who used
// GetKubeconfigWithResponse and got a JSON-decode error instead), or if the
// attribute is not marked sensitive in the plan.
func TestDataSourceKubeconfig_ContentIsByteExactAndSensitive(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c") // freshly created, no StatusSequence set -> reads "running"
	api.SetKubeconfig(cid, seededKubeconfigYAML)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: kubeconfigDataSourceHCL(cid),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectSensitiveValue("data.aether_kubeconfig.k", tfjsonpath.New("content")),
			}},
			Check: resource.TestCheckResourceAttr("data.aether_kubeconfig.k", "content", seededKubeconfigYAML),
		}},
	})
}

// K2. Fails if the provider surfaces the raw API 409 message (or no error at
// all) instead of the T13-11 wrapped wording. The match phrase is
// deliberately the FULL T13-11 wording ("available only while it is
// running"), not the bare word "running" alone: Terraform's own generic
// wrapper text is "Error running pre-apply plan: exit status 1", which would
// make a bare loose("running") pass VACUOUSLY today (before the data source
// even exists) for the wrong reason - caught during authoring by running
// this test and noticing it passed at RED instead of failing.
func TestDataSourceKubeconfig_NonRunningStatusIsAnErrorNamingRunning(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.StatusSequence("*", "upgrading")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      kubeconfigDataSourceHCL(cid),
			ExpectError: loose("available only while it is running"),
		}},
	})
}

// K3. Fails if a 404 (unknown cluster id) is silently swallowed into an empty
// `content` instead of surfacing as an error.
func TestDataSourceKubeconfig_UnknownClusterIs404Error(t *testing.T) {
	api := setupFake(t)
	_ = api
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      kubeconfigDataSourceHCL(zeroUUID),
			ExpectError: loose("not found"),
		}},
	})
}
