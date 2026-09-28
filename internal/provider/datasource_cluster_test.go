package provider_test

import (
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Expected coder symbols: NewClusterDataSource (datasource_cluster.go),
// NewClustersDataSource (datasource_clusters.go).

// Basic read by id. Fails today (RED) with "does not support data source
// \"aether_cluster\"". Once implemented, fails against a wrong
// implementation if the extracted flatten (T13-14: clusterResource.flatten is
// a method, not a free function yet) drops or mis-types any attribute, or if
// `id` is looked up via GetClusterWithResponse but decodes the wrong id.
func TestDataSourceCluster_ByIDReadsEveryClusterAttribute(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "prod")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: clusterDataSourceHCL(cid, ""),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_cluster.c", "id", cid),
				resource.TestCheckResourceAttr("data.aether_cluster.c", "name", "prod"),
				resource.TestCheckResourceAttr("data.aether_cluster.c", "region", "eu-fi"),
				resource.TestCheckResourceAttr("data.aether_cluster.c", "k8s_version", "1.37.2"),
				resource.TestCheckResourceAttr("data.aether_cluster.c", "public_endpoint", "false"),
				resource.TestCheckResourceAttr("data.aether_cluster.c", "status", "running"),
				resource.TestCheckResourceAttr("data.aether_cluster.c", "observed_kubernetes_version", "1.37.2"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "endpoint"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "capi_cluster_name"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "keycloak_realm"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "harbor_project"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "ceph_pool"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "lb_subnet"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "worker_subnet"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "created_at"),
				resource.TestCheckResourceAttrSet("data.aether_cluster.c", "updated_at"),
				resource.TestCheckNoResourceAttr("data.aether_cluster.c", "plan"),
				resource.TestCheckNoResourceAttr("data.aether_cluster.c", "status_message"),
			),
		}},
	})
}

// A 404 by id must be an ERROR, never an empty result (spec §6 / T13-14).
//
// n2 (review round 1): asserts on "404" (the status), not on the fake's
// "not found" message text - loose("not found") would also accept a wrong
// code path that happens to emit the by-name branch's "Cluster not found"
// wording, and the fake's error CODE is "not_found" (underscore, which
// \W+ never matches), so a code-based assertion could not be phrased this
// way at all.
func TestDataSourceCluster_ByIDNotFoundIsAnError(t *testing.T) {
	api := setupFake(t)
	_ = api
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      clusterDataSourceHCL(zeroUUID, ""),
			ExpectError: loose("404"),
		}},
	})
}

// N1 (0 matches): fails if a name with zero matches is silently returned as
// an empty/zero-valued result instead of an error.
func TestDataSourceCluster_ByNameZeroMatchesIsAnError(t *testing.T) {
	api := setupFake(t)
	seedCluster(t, api, "prod")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      clusterDataSourceHCL("", "does-not-exist"),
			ExpectError: loose("no cluster named"),
		}},
	})
}

// N1 (1 match): the ordinary case, by name.
func TestDataSourceCluster_ByNameOneMatchIsFound(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "prod")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: clusterDataSourceHCL("", "prod"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_cluster.c", "id", cid),
				resource.TestCheckResourceAttr("data.aether_cluster.c", "name", "prod"),
			),
		}},
	})
}

// N1 (2 matches, fake-only - unreachable against the real API per
// UNIQUE(org_id,name), kept as defensive coverage per T13-14 point 10): fails
// if the defensive ">1 match" guard is missing and the data source instead
// picks one of the two arbitrarily (which would make this test flaky/green
// by luck) or crashes.
func TestDataSourceCluster_ByNameTwoMatchesIsAnError_FakeOnlyDefensive(t *testing.T) {
	api := setupFake(t)
	// The real API can never produce this (UNIQUE(org_id,name)); the fake has
	// no such constraint, so seeding two clusters with the same name is only
	// possible here - proving the defensive guard exists, not a real scenario.
	seedCluster(t, api, "dup")
	seedCluster(t, api, "dup")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      clusterDataSourceHCL("", "dup"),
			ExpectError: loose("matched 2 clusters"),
		}},
	})
}

// N2: neither id nor name set is a CONFIG error, with zero API calls.
func TestDataSourceCluster_NeitherIDNorNameIsAConfigError(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      clusterDataSourceHCL("", ""),
			ExpectError: exactlyOneOfErrorRE,
		}},
	})
	if reqs := requestsMatching(api, "GET", clustersRE); len(reqs) != 0 {
		t.Fatalf("a config-time validation failure reached the API: %v", reqs)
	}
}

// N2: both id and name set is a CONFIG error, with zero API calls.
func TestDataSourceCluster_BothIDAndNameIsAConfigError(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "prod")
	before := len(api.Requests())
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      clusterDataSourceHCL(cid, "prod"),
			ExpectError: exactlyOneOfErrorRE,
		}},
	})
	if reqs := requestsMatching(api, "GET", oneClusterRE); len(reqs) != 0 {
		t.Fatalf("a config-time validation failure reached the API with a single-cluster GET: %v", reqs)
	}
	if reqs := requestsMatching(api, "GET", clustersRE); len(reqs) != 0 {
		t.Fatalf("a config-time validation failure reached the API with a list GET: %v", reqs)
	}
	if len(api.Requests()) != before {
		t.Fatalf("request count grew from %d to %d despite the config error", before, len(api.Requests()))
	}
}

// P1: aether_clusters walks two full pages plus the trailing empty page.
// Seeds EXACTLY 200 clusters so limit=100 produces [100][100][] - 3 requests
// - and pins T13-1's ruling that requests send limit=100 and the returned
// next_cursor is echoed back as the cursor query param on requests 2 and 3.
//
// Fails if: item count != 200 (collectAll stopped early, wrong stop
// condition, or duplicated a page), any /clusters GET omits "limit=100"
// (T13-1: "requests send limit=100" - a provider sending the framework
// default 50 would still produce a correct 200-item result through MORE
// requests, so the item-count assertion alone cannot catch this - the query
// assertion is load-bearing), or the cursor isn't echoed on requests 2/3.
func TestDataSourceClusters_WalksTwoFullPagesPlusTrailingEmptyPage(t *testing.T) {
	api := setupFake(t)
	ids := api.SeedManyClusters(testOrg, 200)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: dsProviderBlock + `data "aether_clusters" "all" {}`,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("data.aether_clusters.all", "clusters.#", "200"),
				resource.TestCheckResourceAttr("data.aether_clusters.all", "clusters.0.id", ids[0]),
				resource.TestCheckResourceAttr("data.aether_clusters.all", "clusters.199.id", ids[199]),
			),
		}},
	})
	reqs := requestsMatching(api, "GET", clustersRE)
	if len(reqs) == 0 || len(reqs)%3 != 0 {
		t.Fatalf("got %d GET /clusters requests, want a positive multiple of 3 (one [100][100][] walk per Read invocation)", len(reqs))
	}
	last3 := reqs[len(reqs)-3:]
	for _, r := range last3 {
		if !containsQueryParam(r.Query, "limit=100") {
			t.Fatalf("request %q does not carry limit=100 (T13-1): %q", r.Path, r.Query)
		}
	}
	if containsQueryParam(last3[0].Query, "cursor=") {
		t.Fatalf("first request of the walk carries a cursor, want none: %q", last3[0].Query)
	}
	if !containsQueryParam(last3[1].Query, "cursor="+ids[99]) {
		t.Fatalf("second request cursor = %q, want cursor=%s (page 1's last id, echoed as a UUID)", last3[1].Query, ids[99])
	}
	if !containsQueryParam(last3[2].Query, "cursor="+ids[199]) {
		t.Fatalf("third request cursor = %q, want cursor=%s (page 2's last id)", last3[2].Query, ids[199])
	}
}

// containsQueryParam is a substring check over a raw query string - loose on
// purpose (parameter order is not part of the contract).
func containsQueryParam(rawQuery, wantSubstring string) bool {
	return strings.Contains(rawQuery, wantSubstring)
}

// m4 (review round 1, GREEN today - fills a coverage gap, code unchanged):
// parseInputID's rejection happens INSIDE Read today, not in a plan-time
// attribute validator; measured (not assumed) that this test is GREEN
// against current HEAD, contrary to the brief's expectation that it be RED.
// Reason: parseInputID runs before any request is built, and
// resource.UnitTest's harness invokes ReadDataSource during `terraform
// plan` for a fully-known literal config - so "checked by a validator" and
// "checked inside Read" produce an IDENTICAL black-box outcome here (same
// error text, same zero requests). A validator would only diverge from
// this if the config value were unknown at plan time (deferred to apply),
// but attribute validators conventionally skip unknown values too, so no
// black-box test through this harness can pin that distinction. It still
// fills the coverage gap m4 names ("no test pins the rejection") and pins
// both the wording and the zero-requests property as regressions.
func TestDataSourceCluster_UpperCaseIDIsRejectedWithZeroRequests(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      clusterDataSourceHCL(upperCaseUUID, ""),
			ExpectError: loose("canonical"),
		}},
	})
	// requestsMatching, not a total-request-count comparison: Configure's own
	// org auto-resolution (AETHER_ORGANIZATION_ID left empty by setupFake)
	// legitimately issues a request of its own, which a total-count check
	// would misattribute to this test's id.
	if reqs := requestsMatching(api, "GET", oneClusterRE); len(reqs) != 0 {
		t.Fatalf("an invalid id reached the API with a single-cluster GET: %v", reqs)
	}
}

// m3 (review round 1, RED until the coder lands ruling T13-18): a server
// that ignores the cursor and repeats the same next_cursor forever must be a
// PAGING ERROR, never a silently-doubled list. paging.go's collectAll today
// only stops on next == cursor and returns success with whatever it
// collected - here that is page 1 twice - so this fails against the CURRENT
// code (an unexpected apply success where ExpectError wanted a failure),
// not just against a hypothetical wrong implementation. Once T13-18 lands
// (collectAll keeps a `seen` set and errors on any repeated cursor), this
// passes on the "the API repeated a pagination cursor" wording the ruling
// specifies.
func TestDataSourceClusters_RepeatedCursorErrorsInsteadOfDoublingTheList(t *testing.T) {
	api := setupFake(t)
	api.SeedManyClusters(testOrg, 150) // > limit=100, so a first next_cursor is produced
	api.RepeatListCursor(true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      dsProviderBlock + `data "aether_clusters" "all" {}`,
			ExpectError: loose("pagination cursor"),
		}},
	})
}
