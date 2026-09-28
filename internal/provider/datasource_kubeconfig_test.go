package provider_test

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
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
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectSensitiveValue("data.aether_kubeconfig.k", tfjsonpath.New("content")),
			},
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

// m4 (review round 1): an upper-case UUID on cluster_id must be rejected
// with the canonical-UUID wording, with zero API requests. Same measured
// GREEN-today status as the other m4 tests (see datasource_cluster_test.go's
// doc comment): parseInputID already runs before any request inside Read.
func TestDataSourceKubeconfig_UpperCaseClusterIDIsRejectedWithZeroRequests(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      kubeconfigDataSourceHCL(upperCaseUUID),
			ExpectError: loose("canonical"),
		}},
	})
	if reqs := requestsMatching(api, "GET", `/kubeconfig$`); len(reqs) != 0 {
		t.Fatalf("an invalid cluster_id reached the API with a kubeconfig GET: %v", reqs)
	}
}

// m5 (review round 1, GREEN today - fills a coverage gap, code unchanged):
// a 200 with an empty (whitespace-only) body must be an error, not an empty
// `content`.
func TestDataSourceKubeconfig_EmptyBodyIsAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.SetKubeconfig(cid, "   \n")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      kubeconfigDataSourceHCL(cid),
			ExpectError: loose("without a kubeconfig"),
		}},
	})
}

// m5 (review round 1, GREEN today): a 200 body that carries content but no
// "apiVersion" substring is not a kubeconfig and must be an error.
func TestDataSourceKubeconfig_BodyWithoutAPIVersionIsAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.SetKubeconfig(cid, "kind: Config\nclusters: []\n")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      kubeconfigDataSourceHCL(cid),
			ExpectError: loose("without a kubeconfig"),
		}},
	})
}

// m5 (review round 1, GREEN today): a 200 body over the 1 MiB cap must be an
// error, even though it starts with a well-formed "apiVersion" line (proving
// the size check, not the apiVersion check, is what rejects it).
func TestDataSourceKubeconfig_OversizedBodyIsAnError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	oversized := "apiVersion: v1\n" + strings.Repeat("x", 1<<20+1)
	api.SetKubeconfig(cid, oversized)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      kubeconfigDataSourceHCL(cid),
			ExpectError: loose("without a kubeconfig"),
		}},
	})
}

// m5 (review round 1, GREEN today): the API's SECOND 409 cause - the control
// plane has not published a kubeconfig yet even though the cluster already
// reads "running" - must still surface through the T13-11 wrapped wording,
// carrying the API's own message in the parenthetical (notReadyDetail's
// "the API said: ..." branch, since this message has no "Current status:"
// marker to extract from).
func TestDataSourceKubeconfig_ProvisioningConflictOnARunningClusterSurfacesTheAPIMessage(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.SetKubeconfigConflict(cid, "Control plane is still provisioning the kubeconfig")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      kubeconfigDataSourceHCL(cid),
			ExpectError: loose("Control plane is still provisioning"),
		}},
	})
}

// K3. Fails if a 404 (unknown cluster id) is silently swallowed into an empty
// `content` instead of surfacing as an error.
//
// n2 (review round 1): asserts on "404" (the status), not on the fake's
// "not found" message text (loose("not found") tolerates a wrong error
// path too, and cannot be phrased against the "not_found" error CODE at
// all - \W+ never matches an underscore).
func TestDataSourceKubeconfig_UnknownClusterIs404Error(t *testing.T) {
	api := setupFake(t)
	_ = api
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      kubeconfigDataSourceHCL(zeroUUID),
			ExpectError: loose("404"),
		}},
	})
}
