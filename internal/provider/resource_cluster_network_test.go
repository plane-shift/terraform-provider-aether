package provider_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive aether_cluster_network through a real terraform (or tofu)
// binary against the in-process fake, which reproduces what aether-api DOES
// (see fakeapi/networks.go and the SDD task-8 measured contract).
// internal/provider/resource_cluster_network.go landed in 333f06e, and the
// r1 fixes (task-8-review-r1.md M-1/M-2) landed in a256239: every test in
// this file is GREEN. task-8-review-r2.md's proposed NEW-1 test was rejected
// by measurement rather than written - see the M-3 test's comment below for
// why, and task-8-tests-report.md for the full record.
//
// There is no wait/convergence signal anywhere in this API (contract §7), so
// every test here runs at plan/apply speed with no polling and needs no
// timeouts tuning.

const (
	networkResource = "aether_cluster_network.n"
	networkRE       = `/network$`
)

// networkHCL renders one aether_cluster_network resource. cidrs is rendered
// as an explicit HCL list (authorized_cidrs is Required, so every config
// must set it - nil renders `[]`, never an omitted attribute). extra lines
// (e.g. an api_server_rate_limit block) go inside the block.
func networkHCL(clusterID string, cidrs []string, extra ...string) string {
	quoted := make([]string, len(cidrs))
	for i, c := range cidrs {
		quoted[i] = strconv.Quote(c)
	}
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_cluster_network" "n" {
  cluster_id       = %q
  authorized_cidrs = [%s]
%s
}
`, clusterID, strings.Join(quoted, ", "), strings.Join(extra, "\n"))
}

const rateLimitBlock = `
  api_server_rate_limit = {
    max_connections_per_second = 50
    burst                      = 100
  }`

// compactJSON compacts b (as recorded by the fake) for a byte-exact
// assertion. Whitespace-insensitive but key-order-sensitive on purpose: an
// extra or reordered top-level key is exactly what a byte-exact check must
// catch (e.g. a stray api_server_rate_limit key, or authorized_cidrs
// spelled null instead of []).
func compactJSON(t *testing.T, b []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("request body is not compactable JSON: %q: %v", b, err)
	}
	return buf.String()
}

// selfDeletingNotFound returns an http.HandlerFunc that answers 404 exactly
// once, then removes ITSELF from api.Handlers[path] so a later request to the
// same path falls through to the fake's normal routing (used where the
// harness' own automatic post-apply refresh reads the same path again right
// after a step's apply, and must not trip the same forced 404 a second time).
//
// The delete is guarded by a local mutex (task-8-review-r1.md N-4): this
// closure runs on whatever goroutine net/http assigns the connection to,
// which is not necessarily the test's own goroutine, so mutating the shared
// api.Handlers map from inside it is a potential concurrent write. A local
// mutex closes that within THIS test's own accesses to the map; it does not
// establish mutual exclusion with fakeapi's internal serve(), which reads the
// same map under its own unexported lock - closing that fully would need an
// exported synchronization point on fakeapi.Server, out of scope here. A
// targeted `-race` run (task-8-review-r1.md N-4) found no actual race, since
// these requests are issued sequentially in practice.
func selfDeletingNotFound(api *fakeapi.Server, path, message string) http.HandlerFunc {
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delete(api.Handlers, path)
		mu.Unlock()
		fakeapi.WriteJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": message}})
	}
}

// ---------------------------------------------------------------- 1

// Fails if Create sends anything but the two configured CIDRs plus the
// rate-limit block (extra/missing keys, wrong values), if it fails to record
// updated_at/id in state, or if Create polls the network at all (there is no
// convergence signal to wait on - a GET here would be a defect).
func TestClusterNetworkResource_CreateWithCIDRsAndRateLimitSendsExactBodyAndStateMatches(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	cfg := networkHCL(cid, []string{"10.0.0.0/24", "192.168.1.1/32"}, rateLimitBlock)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(networkResource, "cluster_id", cid),
				resource.TestCheckResourceAttr(networkResource, "id", cid),
				resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.#", "2"),
				resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.0", "10.0.0.0/24"),
				resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.1", "192.168.1.1/32"),
				resource.TestCheckResourceAttr(networkResource, "api_server_rate_limit.max_connections_per_second", "50"),
				resource.TestCheckResourceAttr(networkResource, "api_server_rate_limit.burst", "100"),
				resource.TestCheckResourceAttrSet(networkResource, "updated_at"),
				func(*terraform.State) error {
					puts := requestsMatching(api, http.MethodPut, networkRE)
					if len(puts) != 1 {
						return fmt.Errorf("want exactly one PUT, got %d", len(puts))
					}
					body := bodyOf(t, puts[0])
					if got := strings.Join(keysOf(body), ","); got != "api_server_rate_limit,authorized_cidrs" {
						return fmt.Errorf("PUT keys = %s, want exactly api_server_rate_limit,authorized_cidrs: %s", got, puts[0].Body)
					}
					cidrs, ok := body["authorized_cidrs"].([]any)
					if !ok || len(cidrs) != 2 || cidrs[0] != "10.0.0.0/24" || cidrs[1] != "192.168.1.1/32" {
						return fmt.Errorf("PUT authorized_cidrs = %v, want the two configured CIDRs in order", body["authorized_cidrs"])
					}
					rl, ok := body["api_server_rate_limit"].(map[string]any)
					if !ok {
						return fmt.Errorf("PUT body has no api_server_rate_limit object: %s", puts[0].Body)
					}
					if err := expectNumber(rl, "max_connections_per_second", 50); err != nil {
						return err
					}
					if err := expectNumber(rl, "burst", 100); err != nil {
						return err
					}
					if gets := len(requestsMatching(api, http.MethodGet, networkRE)); gets != 0 {
						return fmt.Errorf("create GET the network %d time(s); there is no convergence signal, so Create must never poll it", gets)
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- 2

// Fails if an empty configured list is ever sent as `null` (the fake 500s
// that, per contract §3/§11) instead of an explicit `[]`, or if the provider
// invents an api_server_rate_limit default instead of leaving it null.
func TestClusterNetworkResource_CreateWithEmptyCIDRsAndNoRateLimitSendsExplicitEmptyList(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: networkHCL(cid, nil),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.#", "0"),
				resource.TestCheckNoResourceAttr(networkResource, "api_server_rate_limit.%"),
				func(*terraform.State) error {
					puts := requestsMatching(api, http.MethodPut, networkRE)
					if len(puts) != 1 {
						return fmt.Errorf("want exactly one PUT, got %d", len(puts))
					}
					want := `{"authorized_cidrs":[]}`
					if got := compactJSON(t, puts[0].Body); got != want {
						return fmt.Errorf("create PUT body = %s, want exactly %s (a nil/null slice is a 500 on the real API and the fake)", got, want)
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- 3

// Fails if Update collapses to a merge (only sending the delta) instead of a
// full replace, or reorders/drops entries instead of sending the config's
// own order.
func TestClusterNetworkResource_UpdateReordersAndChangesTheListInConfigOrder(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: networkHCL(cid, []string{"10.0.0.0/24", "192.168.1.0/24"})},
			{
				Config: networkHCL(cid, []string{"192.168.1.0/24", "10.0.0.0/24", "172.16.0.0/16"}),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkResource, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.0", "192.168.1.0/24"),
					resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.1", "10.0.0.0/24"),
					resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.2", "172.16.0.0/16"),
					func(*terraform.State) error {
						body, err := lastBody(t, api, http.MethodPut, networkRE)
						if err != nil {
							return err
						}
						cidrs, ok := body["authorized_cidrs"].([]any)
						if !ok || len(cidrs) != 3 || cidrs[0] != "192.168.1.0/24" || cidrs[1] != "10.0.0.0/24" || cidrs[2] != "172.16.0.0/16" {
							return fmt.Errorf("PUT authorized_cidrs = %v, want the config's own order preserved (List semantics, no reordering)", body["authorized_cidrs"])
						}
						return nil
					},
					// M-5 (task-8-review-r1.md): Update's "0 GETs" property had no
					// test - only Create's did. step.Check runs right after this
					// step's own apply and before the harness' unconditional
					// post-apply "perpetual diff" refresh (which DOES GET, and is
					// not what this measures) - but requestsMatching accumulates
					// over the WHOLE test, including step 1's create and its own
					// end-of-step refresh cycle, so a plain "count all GETs == 0"
					// would always be nonzero by step 2. Instead: find the LAST PUT
					// to this path (step 2's own Update) and count GETs of it AFTER
					// that point only.
					func(*terraform.State) error {
						reqs := api.Requests()
						lastPUT := -1
						for i, r := range reqs {
							if r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/network") {
								lastPUT = i
							}
						}
						if lastPUT == -1 {
							return fmt.Errorf("no PUT of the network was recorded at all")
						}
						gets := 0
						for _, r := range reqs[lastPUT+1:] {
							if r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/network") {
								gets++
							}
						}
						if gets != 0 {
							return fmt.Errorf("update GET the network %d time(s) after its own PUT; Update must never poll after its PUT", gets)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 4

// Fails if removing the rate-limit block from config either keeps sending
// the old value (omission must RESET it, contract §3), sends an explicit
// zero object (a 422 on the real API), or produces an inconsistent result
// because state was not cleared to null.
func TestClusterNetworkResource_RemovingTheRateLimitBlockOmitsItFromTheWireAndClearsState(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	withRL := networkHCL(cid, []string{"10.0.0.0/24"}, rateLimitBlock)
	withoutRL := networkHCL(cid, []string{"10.0.0.0/24"})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: withRL, Check: resource.TestCheckResourceAttr(networkResource, "api_server_rate_limit.burst", "100")},
			{
				Config: withoutRL,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr(networkResource, "api_server_rate_limit.%"),
					func(*terraform.State) error {
						body, err := lastBody(t, api, http.MethodPut, networkRE)
						if err != nil {
							return err
						}
						if v, has := body["api_server_rate_limit"]; has {
							return fmt.Errorf("PUT body carries api_server_rate_limit = %v, want the key entirely absent (omission resets it; the API would 422 a zero object)", v)
						}
						return nil
					},
				),
			},
			{Config: withoutRL, PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

// ---------------------------------------------------------------- 5

// Fails if adding the rate-limit block back after it was removed does not
// reach state (e.g. a stale UseStateForUnknown-style planning bug that never
// updates a null block).
func TestClusterNetworkResource_AddingTheRateLimitBlockBackIsReflectedInState(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	withoutRL := networkHCL(cid, []string{"10.0.0.0/24"})
	withRL := networkHCL(cid, []string{"10.0.0.0/24"}, `
  api_server_rate_limit = {
    max_connections_per_second = 20
    burst                      = 40
  }`)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: withoutRL, Check: resource.TestCheckNoResourceAttr(networkResource, "api_server_rate_limit.%")},
			{
				Config: withRL,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(networkResource, "api_server_rate_limit.max_connections_per_second", "20"),
					resource.TestCheckResourceAttr(networkResource, "api_server_rate_limit.burst", "40"),
				),
			},
		},
	})
	// N-3 (task-8-review-r1.md): `len(api.Requests()) == 0` can never fail
	// once any step has run - replaced with a count that pins the real
	// property: one PUT per apply step (create without a rate limit, then
	// update adding one back) plus resource.UnitTest's own end-of-test
	// teardown destroy PUT (see the comment on test 6/DeleteSendsExactly...
	// for why the destroy is always a third PUT here, not a DELETE call).
	if n := len(requestsMatching(api, http.MethodPut, networkRE)); n != 3 {
		t.Fatalf("want exactly 3 PUTs (create, update, teardown destroy), got %d", n)
	}
}

// ---------------------------------------------------------------- 6

// Fails if Delete sends anything other than exactly {"authorized_cidrs":[]}
// (e.g. the plan's originally-documented zero rate-limit object, which the
// fake 422s per contract §6 - a resource shaped that way could never
// destroy successfully).
func TestClusterNetworkResource_DeleteSendsExactlyEmptyCIDRsWithNoRateLimitKey(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: networkHCL(cid, []string{"10.0.0.0/24"}, rateLimitBlock),
		}},
	})
	puts := requestsMatching(api, http.MethodPut, networkRE)
	if len(puts) != 2 {
		t.Fatalf("want the create PUT plus the harness' final destroy PUT, got %d", len(puts))
	}
	last := puts[len(puts)-1]
	want := `{"authorized_cidrs":[]}`
	if got := compactJSON(t, last.Body); got != want {
		t.Fatalf("destroy PUT body = %s, want exactly %s (a zero rate-limit object is a 422 on the real API and the fake; destroy could never succeed with it)", got, want)
	}
}

// ---------------------------------------------------------------- 7

// Fails if importing a cluster that was never PUT does not read back the
// synthetic §2 default (empty list, null rate limit), if `id` is not set to
// the cluster id, or if import itself issues any PUT (adopt-only). The PUT
// count is asserted INSIDE ImportStateCheck, which runs right after the
// import completes and before resource.UnitTest's own end-of-test teardown
// destroy - that destroy PUTs the T8-1 delete body `{"authorized_cidrs":[]}`
// for the resource ImportStatePersist just wrote into state, and counting it
// here would wrongly fail (or wrongly pass) on a request the import itself
// never made.
func TestClusterNetworkResource_ImportOfANeverConfiguredClusterReadsEmptyCIDRsAndNullRateLimit(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c") // never PUT: the network row does not exist yet
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				ResourceName: networkResource, ImportState: true, ImportStatePersist: true,
				Config:        networkHCL(cid, nil),
				ImportStateId: cid,
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					if len(is) != 1 {
						return fmt.Errorf("want exactly one imported instance, got %d", len(is))
					}
					if got := is[0].Attributes["authorized_cidrs.#"]; got != "0" {
						return fmt.Errorf("imported authorized_cidrs.# = %q, want 0 (the never-configured default, §2)", got)
					}
					if v, has := is[0].Attributes["api_server_rate_limit.%"]; has {
						return fmt.Errorf("imported api_server_rate_limit present (%%=%s), want null/absent", v)
					}
					if got := is[0].Attributes["id"]; got != cid {
						return fmt.Errorf("imported id = %q, want the cluster id %q", got, cid)
					}
					// M-5 (task-8-review-r1.md): the zero-time -> RFC3339 text
					// flattening had no test at resource level; a regression that
					// nulled it instead would pass silently otherwise.
					if got := is[0].Attributes["updated_at"]; got != "0001-01-01T00:00:00Z" {
						return fmt.Errorf("imported updated_at = %q, want the zero-time RFC3339 text (not null) for a never-configured cluster (§2)", got)
					}
					if reqs := requestsMatching(api, http.MethodPut, networkRE); len(reqs) != 0 {
						return fmt.Errorf("import must never PUT (adopt-only), got %d PUT(s) by the time the import finished: %v", len(reqs), reqs)
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- 8

// Fails if a 404 on Read (the CLUSTER is gone) does not remove the resource
// from state - the next plan must then recreate it, not error or no-op. The
// cluster is kept registered on the fake and only the network GET is forced
// to 404 exactly once (a Handlers override that deletes itself on first use):
// a genuinely removed cluster (api.RemoveCluster) would also make the
// recreate's own PUT 404, so the test could never observe whether Create
// actually succeeds after the removal - only that a PUT was attempted. This
// still fails exactly when the brief requires: if Read treats 404 as an
// error instead of removing the resource, the pre-apply plan/refresh errors
// out (a genuine test failure) instead of ever reaching the plan check; if
// Read instead leaves the resource in state (ignores the 404), the plan
// check below sees NoOp/Update instead of Create.
func TestClusterNetworkResource_Read404RemovesFromStateAndNextPlanRecreates(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	cfg := networkHCL(cid, []string{"10.0.0.0/24"})
	networkGetPath := "GET /organizations/" + testOrg + "/clusters/" + cid + "/network"
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					// One-shot: self-deletes on first use, so the harness' own
					// automatic post-apply "perpetual diff" refresh (which reads
					// this same path again right after Create) sees the freshly
					// created row instead of tripping the same 404 a second time
					// and wrongly wiping the just-created resource from state.
					api.Handlers[networkGetPath] = selfDeletingNotFound(api, networkGetPath, "Resource not found")
				},
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkResource, plancheck.ResourceActionCreate),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.#", "1"),
					resource.TestCheckResourceAttr(networkResource, "authorized_cidrs.0", "10.0.0.0/24"),
					func(*terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, networkRE)
						if len(puts) != 2 {
							return fmt.Errorf("want the original create PUT plus the recreate's PUT, got %d", len(puts))
						}
						return nil
					},
				),
			},
		},
	})
}

// Fails if a 403 on Read (org mismatch, never "the policy is unset") is
// treated like a 404 (removing the resource from state) instead of
// surfacing as an error while keeping state intact - checked by a
// subsequent PlanOnly step that must see no diff once the 403 stops firing.
func TestClusterNetworkResource_Read403IsAnErrorAndKeepsState(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	cfg := networkHCL(cid, []string{"10.0.0.0/24"})
	forbiddenRoute := "GET /organizations/" + testOrg + "/clusters/" + cid + "/network"
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					api.Handlers[forbiddenRoute] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "forbidden", "message": "org mismatch on cluster network read"}})
					}
				},
				Config:      cfg,
				ExpectError: loose("org mismatch on cluster network read"),
			},
			{
				// the 403 must not have removed the resource from state: once it
				// stops firing, a plain refresh/plan must see no drift at all
				PreConfig: func() { delete(api.Handlers, forbiddenRoute) },
				Config:    cfg, PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// ---------------------------------------------------------------- 9

// Each case must be refused at PLAN time, before any network-route request
// is made - a cluster id that was never even seeded on the fake proves the
// validator runs before any lookup could occur. Fails if the provider drops
// any of these checks (accepts IPv6, accepts host bits, accepts a duplicate,
// or accepts a rate-limit value outside 1..2147483647).
func TestClusterNetworkResource_PlanTimeValidatorsRefuseWithoutAnyNetworkRequest(t *testing.T) {
	const neverSeededCluster = "11111111-1111-4111-8111-111111111111"
	cases := []struct {
		name, cfg, errRE string
	}{
		{"bare address (not CIDR notation)", networkHCL(neverSeededCluster, []string{"10.0.0.1"}), `(?is)CIDR`},
		{"IPv6 is not accepted here", networkHCL(neverSeededCluster, []string{"2001:db8::/32"}), `(?is)IPv4`},
		{"host bits (not the network address)", networkHCL(neverSeededCluster, []string{"10.0.0.1/8"}), `(?is)network address|host bits`},
		{"duplicate entries", networkHCL(neverSeededCluster, []string{"10.0.0.0/24", "10.0.0.0/24"}), `(?is)duplicate`},
		// Deliberately tolerant of either a single Between(1, 2147483647) validator
		// or a pair of AtLeast(1)/AtMost(2147483647) validators - the brief pins
		// the bounds, not the validator composition. A bare "1" alone is NOT
		// used as the pattern: it would spuriously match unrelated text (e.g.
		// "exit status 1" from an unimplemented resource) - caught during
		// authoring by getting a false PASS before the resource even existed.
		{"rate limit at the floor (0 is rejected, not unlimited)", networkHCL(neverSeededCluster, nil, `
  api_server_rate_limit = {
    max_connections_per_second = 0
    burst                      = 10
  }`), `(?is)at\s+least\s+1\b|between\s+1\s+and\s+2147483647`},
		{"rate limit above int32", networkHCL(neverSeededCluster, nil, `
  api_server_rate_limit = {
    max_connections_per_second = 10
    burst                      = 2147483648
  }`), `(?is)2147483647`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{{
					Config:      c.cfg,
					ExpectError: regexp.MustCompile(c.errRE),
				}},
			})
			if reqs := requestsMatching(api, http.MethodGet, networkRE); len(reqs) != 0 {
				t.Fatalf("%s: a plan-time validation failure reached the API with a GET: %v", c.name, reqs)
			}
			if reqs := requestsMatching(api, http.MethodPut, networkRE); len(reqs) != 0 {
				t.Fatalf("%s: a plan-time validation failure reached the API with a PUT: %v", c.name, reqs)
			}
		})
	}
}

// ---------------------------------------------------------------- 10

// Fails if changing cluster_id is treated as an in-place update (it must
// destroy the old cluster's policy - opening its endpoint - and create a new
// one, never a PUT against the new cluster_id with the old resource's id).
func TestClusterNetworkResource_ClusterIDChangePlansReplace(t *testing.T) {
	api := setupFake(t)
	cidA := seedCluster(t, api, "a")
	cidB := seedCluster(t, api, "b")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: networkHCL(cidA, []string{"10.0.0.0/24"})},
			{
				Config: networkHCL(cidB, []string{"10.0.0.0/24"}),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkResource, plancheck.ResourceActionReplace),
				}},
			},
		},
	})
}

// ---------------------------------------------------------------- r1 gaps (task-8-review-r1.md M-5)

// Fails if Delete treats a 404 on its own PUT as an error instead of success
// (the CLUSTER is already gone, contract §8 - there is nothing left to
// reset). Forced via a Handlers override on the OLD cluster's PUT path only,
// exercised through a cluster_id replace so the override lands on exactly the
// DELETE half of the apply: the new cluster's create PUT is a different path
// (a different cluster id) and is never touched by it, so the test can tell
// "Delete succeeded on a 404" from "the whole apply failed for some other
// reason" - a genuinely removed cluster (api.RemoveCluster) would make BOTH
// halves 404 and could not distinguish the two.
func TestClusterNetworkResource_DeletePUTAnswering404OnAnAlreadyGoneClusterIsSuccess(t *testing.T) {
	api := setupFake(t)
	cidA := seedCluster(t, api, "a")
	cidB := seedCluster(t, api, "b")
	oldPutPath := "PUT /organizations/" + testOrg + "/clusters/" + cidA + "/network"
	oldPutRE := "/clusters/" + cidA + "/network$"
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: networkHCL(cidA, []string{"10.0.0.0/24"})},
			{
				PreConfig: func() {
					api.Handlers[oldPutPath] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "Resource not found"}})
					}
				},
				Config: networkHCL(cidB, []string{"10.0.0.0/24"}),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(networkResource, plancheck.ResourceActionReplace),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(networkResource, "cluster_id", cidB),
					func(*terraform.State) error {
						// oldPutRE also matches step 1's own create PUT for cidA (same
						// path, same cluster): 2 total is create + this step's destroy.
						if n := len(requestsMatching(api, http.MethodPut, oldPutRE)); n != 2 {
							return fmt.Errorf("want exactly 2 PUTs on the old cluster's path (the original create plus the destroy that got 404'd), got %d", n)
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() { delete(api.Handlers, oldPutPath) },
				Config:    networkHCL(cidB, []string{"10.0.0.0/24"}), PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// Fails if a non-200 PUT is swallowed instead of surfacing the API's own
// message, or if any state is left behind despite the failure. Forced via a
// Handlers override (the plan-time validators make a real 422 unreachable
// through normal config, and the fake's other error shapes are all driven by
// the request BODY, which the resource always builds correctly - so a raw
// override is the only way to exercise this path at all). The second step's
// empty-config PlanOnly proves nothing was persisted: Create never calls
// resp.State.Set before checking for an error today, but a regression that
// reordered those two calls would leave an orphaned resource, and the empty
// config would then plan a non-empty (destroy) diff for it.
func TestClusterNetworkResource_CreateSurfacesAPUTErrorAndLeavesNoState(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	putPath := "PUT /organizations/" + testOrg + "/clusters/" + cid + "/network"
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					api.Handlers[putPath] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal_error", "message": "simulated PUT failure for test"}})
					}
				},
				Config:      networkHCL(cid, []string{"10.0.0.0/24"}),
				ExpectError: loose("simulated PUT failure for test"),
			},
			{
				PreConfig: func() { delete(api.Handlers, putPath) },
				Config:    `provider "aether" {}`,
				PlanOnly:  true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// ---------------------------------------------------------------- r1 fixes (task-8-review-r1.md M-1, M-2) - GREEN since a256239
//
// Message substrings were coordinated with the coder via
// task-8-tests-report.md: M-1's fix produces an error containing
// "must be a canonical lower-case UUID" (exact substring, matched below);
// M-2's message was left to the coder's choice among the concepts already in
// use on this validator (canonical / network address / host bits).

// M-1: a non-canonical cluster_id (upper case) is refused at PLAN time, with
// zero network-route HTTP calls. Before a256239, parseID accepted it and
// flatten silently overwrote ClusterID with the canonical form AFTER a real
// PUT had already happened ("Provider produced inconsistent result after
// apply", task-8-review-r1.md M-1); a256239 added canonicalUUIDValidator on
// cluster_id, which this test now confirms fires before any request.
func TestClusterNetworkResource_UpperCaseClusterIDIsRefusedAtPlanWithZeroCalls(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	upper := strings.ToUpper(cid)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      networkHCL(upper, []string{"10.0.0.0/24"}),
			ExpectError: regexp.MustCompile(`(?is)must be a canonical lower-case UUID`),
		}},
	})
	if reqs := requestsMatching(api, http.MethodGet, networkRE); len(reqs) != 0 {
		t.Fatalf("an upper-case cluster_id reached the API with a GET: %v", reqs)
	}
	if reqs := requestsMatching(api, http.MethodPut, networkRE); len(reqs) != 0 {
		t.Fatalf("an upper-case cluster_id reached the API with a PUT: %v", reqs)
	}
}

// M-2: `10.0.0.0/08` parses successfully (go1.26.5 net.ParseCIDR accepts a
// leading-zero prefix length) and has no host bits set, so the validator
// would otherwise accept it - but the platform's cluster schema prefix-length
// pattern `([0-9]|[12][0-9]|3[0-2])$` does not match "08", so the merge patch
// is expected to be refused there and the CP-proxy would keep enforcing the
// OLD policy (contract §7.3): exactly the divergence this validator exists to
// prevent. a256239 added the canonical-string-form check (`ipnet.String() ==
// entry`) this test confirms.
func TestClusterNetworkResource_NonCanonicalPrefixLengthIsRefusedAtPlanWithZeroCalls(t *testing.T) {
	const neverSeededCluster = "11111111-1111-4111-8111-111111111111"
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      networkHCL(neverSeededCluster, []string{"10.0.0.0/08"}),
			ExpectError: regexp.MustCompile(`(?is)canonical|network address|host bits`),
		}},
	})
	if reqs := requestsMatching(api, http.MethodGet, networkRE); len(reqs) != 0 {
		t.Fatalf("a non-canonical prefix length reached the API with a GET: %v", reqs)
	}
	if reqs := requestsMatching(api, http.MethodPut, networkRE); len(reqs) != 0 {
		t.Fatalf("a non-canonical prefix length reached the API with a PUT: %v", reqs)
	}
}

// ---------------------------------------------------------------- r3 (task-8-review-r2.md M-3)

// M-3. `${substr(timestamp(), 0, 0)}10.0.0.0/08` plans as `(known after apply)` - timestamp() is a
// built-in Terraform marks unknown at plan on both engines regardless of its arguments, and
// substr(...) of an unknown value is itself unknown - while the LIST stays known (only the one
// element is unknown), so the plan-time cidrListValidator skips this entry at the INITIAL plan (it
// only checks entries already known, resource_cluster_network.go:227-231). At apply the expression
// resolves to the literal "10.0.0.0/08" - the same non-canonical-prefix-length defect as M-2 - so
// this asserts the SAME "canonical form" error is still refused, with zero requests reaching the API.
//
// What this test actually proves (measured, not assumed - see task-8-tests-report.md for the full
// record): it is Terraform CORE's own apply-time behaviour that catches this, not put()'s manual
// apply-time re-check (the checkCIDRs call inside put()). Terraform re-invokes the provider's
// ValidateResourceConfig (and PlanResourceChange) against the now-resolved config as part of its own
// "plan for apply" step, for ANY attribute that was unknown at the initial plan and becomes known
// before apply - this is standard core behaviour for cross-resource and function-derived unknowns
// alike, not something specific to this provider or to timestamp(). Confirmed two ways: (1) the
// failure surfaces as "Error running apply", never "Error running pre-apply plan" - proving the
// value was genuinely unknown through the initial plan and only re-checked once resolved; (2)
// deleting the `if !diags.HasError() { ... checkCIDRs ... }` block guarding the checkCIDRs call
// inside put() and rebuilding left this test GREEN, unchanged - cidrListValidator alone, re-invoked
// by core, still refuses the value before any request. put()'s own recheck is real defense-in-depth
// (it also protects a hypothetical direct API-client caller of this Go package, and costs nothing),
// but this test cannot and does not isolate it - no test can, since core's
// re-validation is unconditional for this HCL shape. This is why NEW-1 (an equivalent apply-time
// bounds recheck proposed for the rate limit) was rejected rather than written: the identical
// mechanism already re-invokes int64validator.Between on the resolved value, so no HCL scenario
// reaches put() with a bad rate limit either, and a test for a fix that cannot fail is forbidden.
func TestClusterNetworkResource_ApplyTimeCIDRRecheckRefusesAnEntryUnknownAtPlan(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      networkHCL(cid, []string{`${substr(timestamp(), 0, 0)}10.0.0.0/08`}),
			ExpectError: regexp.MustCompile(`(?is)canonical form`),
		}},
	})
	if reqs := requestsMatching(api, http.MethodGet, networkRE); len(reqs) != 0 {
		t.Fatalf("an initially-unknown CIDR defect reached the API with a GET: %v", reqs)
	}
	if reqs := requestsMatching(api, http.MethodPut, networkRE); len(reqs) != 0 {
		t.Fatalf("an initially-unknown CIDR defect reached the API with a PUT: %v", reqs)
	}
}
