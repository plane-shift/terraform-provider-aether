package provider_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive aether_node_pool through a real terraform (or tofu) binary
// against the in-process fake, which reproduces what aether-api DOES (see
// fakeapi/nodepools.go). Every config carries SHORT timeouts so a regression
// that never converges fails in seconds, not after the 30 m default.

const (
	poolResource = "aether_node_pool.p"
	poolsRE      = `/nodepools$`
	onePoolRE    = `/nodepools/[^/]+$`
	shortTimeout = `timeouts = { create = "20s", update = "20s", delete = "20s" }`
)

// seedCluster creates a cluster straight through the fake's HTTP API (still
// "provisioning": the API has no cluster-status precondition for pools).
func seedCluster(t *testing.T, api *fakeapi.Server, name string) string {
	t.Helper()
	return seedJSON(t, api, "/organizations/"+testOrg+"/clusters", fmt.Sprintf(`{"name":%q,"region":"eu-fi","k8s_version":"1.37"}`, name))
}

func seedPool(t *testing.T, api *fakeapi.Server, clusterID, name string) string {
	t.Helper()
	return seedJSON(t, api, "/organizations/"+testOrg+"/clusters/"+clusterID+"/nodepools",
		fmt.Sprintf(`{"name":%q,"vcpus":2,"memory_gb":4,"disk_gb":20}`, name))
}

func seedJSON(t *testing.T, api *fakeapi.Server, path, body string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, api.URL()+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+api.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("seeding POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("seeding POST %s = %d %s, want 202", path, resp.StatusCode, b)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ID == "" {
		t.Fatalf("seeding POST %s: no id in %s", path, b)
	}
	return out.ID
}

// poolBlock renders one aether_node_pool resource; extra lines go inside the
// block. Short timeouts are added unless the extras carry their own.
func poolBlock(res, clusterID, name string, vcpus, mem, disk int, extra ...string) string {
	body := strings.Join(extra, "\n")
	if !strings.Contains(body, "timeouts") {
		body += "\n" + shortTimeout
	}
	return fmt.Sprintf(`
resource "aether_node_pool" %q {
  cluster_id = %q
  name       = %q
  vcpus      = %d
  memory_gb  = %d
  disk_gb    = %d
%s
}
`, res, clusterID, name, vcpus, mem, disk, body)
}

func poolHCL(clusterID, name string, extra ...string) string {
	return `provider "aether" {}` + poolBlock("p", clusterID, name, 2, 4, 20, extra...)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lastBody(t *testing.T, api *fakeapi.Server, method, re string) (map[string]any, error) {
	t.Helper()
	rs := requestsMatching(api, method, re)
	if len(rs) == 0 {
		return nil, fmt.Errorf("no %s %s request was sent", method, re)
	}
	return bodyOf(t, rs[len(rs)-1]), nil
}

func expectNumber(body map[string]any, key string, want float64) error {
	if v, ok := body[key].(float64); !ok || v != want {
		return fmt.Errorf("body %v: %s = %v, want %v", body, key, body[key], want)
	}
	return nil
}

func forbidKeys(body map[string]any, keys ...string) error {
	for _, k := range keys {
		if _, has := body[k]; has {
			return fmt.Errorf("request body carries %q, which must never be sent: %v", k, body)
		}
	}
	return nil
}

// ---------------------------------------------------------------- 1

func TestNodePoolResource_FixedPoolCreateWaitsForRunningAndNormalisesTheAPIsDefaults(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.NodePoolStatusSequence("*", "provisioning", "provisioning", "running")
	cfg := poolHCL(cid, "workers")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(poolResource, "status", "running"),
				resource.TestCheckResourceAttr(poolResource, "current_nodes", "1"),
				resource.TestCheckResourceAttr(poolResource, "desired_nodes", "1"),
				resource.TestCheckResourceAttr(poolResource, "min_nodes", "1"),
				resource.TestCheckResourceAttr(poolResource, "max_nodes", "1"),
				resource.TestCheckResourceAttr(poolResource, "min_vcpus", "0"),
				resource.TestCheckResourceAttr(poolResource, "max_vcpus", "0"),
				resource.TestCheckResourceAttr(poolResource, "min_memory_gb", "0"),
				resource.TestCheckResourceAttr(poolResource, "max_memory_gb", "0"),
				resource.TestCheckResourceAttr(poolResource, "cluster_id", cid),
				resource.TestCheckResourceAttrSet(poolResource, "id"),
				resource.TestCheckResourceAttrSet(poolResource, "first_ready_at"),
				// the API's {} and [] must not leak into state when the config left them null
				resource.TestCheckNoResourceAttr(poolResource, "node_labels.%"),
				resource.TestCheckNoResourceAttr(poolResource, "node_taints.#"),
				resource.TestCheckNoResourceAttr(poolResource, "drain_timeout_seconds"),
				func(*terraform.State) error {
					posts := requestsMatching(api, http.MethodPost, poolsRE)
					if len(posts) != 1 {
						return fmt.Errorf("want one create, got %d", len(posts))
					}
					body := bodyOf(t, posts[0])
					if err := forbidKeys(body, "drain_timeout_seconds", "scale_down_delay_seconds", "scale_down_utilization_percent", "node_labels", "node_taints"); err != nil {
						return err
					}
					for k, want := range map[string]float64{"vcpus": 2, "memory_gb": 4, "disk_gb": 20} {
						if err := expectNumber(body, k, want); err != nil {
							return err
						}
					}
					if body["name"] != "workers" {
						return fmt.Errorf("create body name = %v", body["name"])
					}
					// waited: POST, then GETs until the sequence reached running
					if gets := len(requestsMatching(api, http.MethodGet, onePoolRE)); gets < 3 {
						return fmt.Errorf("create returned after %d GET(s) of the pool, want >= 3 (provisioning, provisioning, running)", gets)
					}
					return nil
				},
			),
		}, {
			// the API's echo of the defaults is not drift
			Config: cfg, PlanOnly: true, ExpectNonEmptyPlan: false,
		}},
	})
}

// ---------------------------------------------------------------- 2

func TestNodePoolResource_CreatePollsThroughDegradedAndSucceeds(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.NodePoolStatusSequence("*", "provisioning", "degraded", "running")
	api.SetNodePoolStatusMessage("*", "Available=False: MinimumReplicasUnavailable")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: poolHCL(cid, "workers"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(poolResource, "status", "running"),
				resource.TestCheckResourceAttr(poolResource, "current_nodes", "1"),
				// the message belongs to the degraded read only
				resource.TestCheckNoResourceAttr(poolResource, "status_message"),
			),
		}},
	})
}

func TestNodePoolResource_APoolThatNeverLeavesDegradedFailsAtTheCreateTimeoutWithItsMessage(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.NodePoolStatusSequence("*", "provisioning", "degraded")
	api.SetNodePoolStatusMessage("*", "Available=False: no capacity for 2 vCPU nodes")
	started := time.Now()
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      poolHCL(cid, "workers", `timeouts = { create = "6s" }`),
			ExpectError: loose("no capacity for 2 vCPU nodes"),
		}, {
			// the timed-out pool is in state (tainted); let the harness clean it up
			PreConfig: func() {
				if d := time.Since(started); d > 60*time.Second {
					t.Errorf("the create timeout of 6s took %s to fire", d)
				}
				api.NodePoolStatusSequence("*", "running")
			},
			Config: poolHCL(cid, "workers"),
		}},
	})
}

// M-2 (review round 1): "deleting" mid-create (an out-of-band delete racing
// the create wait) must fail fast naming the status, never burn the create
// timeout - waitForPool's Failed list already includes "deleting" (choice 2,
// ACCEPT in the round-1 review); this pins it with a bounded elapsed time.
func TestNodePoolResource_APoolThatGoesDeletingMidCreateFailsFastNamingDeleting(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.NodePoolStatusSequence("*", "provisioning", "provisioning", "deleting")
	started := time.Now()
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      poolHCL(cid, "workers", `timeouts = { create = "20s" }`),
			ExpectError: loose("reached deleting"),
		}},
	})
	if d := time.Since(started); d > 15*time.Second {
		t.Errorf("a pool that went deleting mid-create took %s to fail, want well under the 20s create timeout (it should fail on the probe that first sees deleting, not by timing out)", d)
	}
}

// ---------------------------------------------------------------- 3

func TestNodePoolResource_ScalingAFixedPoolSendsAllThreeCountsAndWaitsForConvergence(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "workers", "desired_nodes = 1")},
			{
				Config: poolHCL(cid, "workers", "desired_nodes = 3"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(poolResource, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeTestCheckFunc(
					// the API's status is still "running" straight after the PUT; only
					// current_nodes == desired_nodes proves the wait ran to convergence
					resource.TestCheckResourceAttr(poolResource, "current_nodes", "3"),
					resource.TestCheckResourceAttr(poolResource, "status", "running"),
					resource.TestCheckResourceAttr(poolResource, "desired_nodes", "3"),
					resource.TestCheckResourceAttr(poolResource, "min_nodes", "3"),
					resource.TestCheckResourceAttr(poolResource, "max_nodes", "3"),
					func(*terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, onePoolRE)
						if len(puts) != 1 {
							return fmt.Errorf("want exactly one PUT, got %d", len(puts))
						}
						body := bodyOf(t, puts[0])
						if got := strings.Join(keysOf(body), ","); got != "desired_nodes,max_nodes,min_nodes" {
							return fmt.Errorf("PUT keys = %s, want exactly the three node counts: %s", got, puts[0].Body)
						}
						for _, k := range []string{"min_nodes", "desired_nodes", "max_nodes"} {
							if err := expectNumber(body, k, 3); err != nil {
								return err
							}
						}
						return nil
					},
				),
			},
			{Config: poolHCL(cid, "workers", "desired_nodes = 3"), PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

func TestNodePoolResource_ScalingKeepsAnExplicitMinAndMaxAndSendsThemWithDesired(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "workers", "min_nodes = 1", "desired_nodes = 1", "max_nodes = 5")},
			{
				Config: poolHCL(cid, "workers", "min_nodes = 1", "desired_nodes = 3", "max_nodes = 5"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "current_nodes", "3"),
					func(*terraform.State) error {
						body, err := lastBody(t, api, http.MethodPut, onePoolRE)
						if err != nil {
							return err
						}
						// a desired-only PUT would be a 422 (or collapse the bounds): all three ride together
						for k, want := range map[string]float64{"min_nodes": 1, "desired_nodes": 3, "max_nodes": 5} {
							if err := expectNumber(body, k, want); err != nil {
								return err
							}
						}
						return forbidKeys(body, "name", "vcpus", "memory_gb", "disk_gb", "node_labels", "node_taints")
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 4

const autoscalingBudget = `min_vcpus = 2
max_vcpus = 8
min_memory_gb = 4
max_memory_gb = 16`

func TestNodePoolResource_AutoscalingBudgetChangePutsOnlyThatFieldAndDoesNotWait(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	raised := strings.Replace(autoscalingBudget, "max_vcpus = 8", "max_vcpus = 12", 1)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "auto", autoscalingBudget), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(poolResource, "max_vcpus", "8"),
				resource.TestCheckResourceAttr(poolResource, "status", "running"),
			)},
			{
				// A budget change has no convergence signal in the API: the pool
				// reads degraded here and the apply must still return promptly
				// (a wait for "running" would run into the 20s update timeout).
				PreConfig: func() {
					api.NodePoolStatusSequence("*", "degraded")
					api.SetNodePoolStatusMessage("*", "Available=False: MinimumReplicasUnavailable")
				},
				Config: poolHCL(cid, "auto", raised),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "max_vcpus", "12"),
					func(*terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, onePoolRE)
						if len(puts) != 1 {
							return fmt.Errorf("want one PUT, got %d", len(puts))
						}
						body := bodyOf(t, puts[0])
						if err := expectNumber(body, "max_vcpus", 12); err != nil {
							return err
						}
						return forbidKeys(body, "name", "vcpus", "memory_gb", "disk_gb", "node_labels", "node_taints",
							"min_nodes", "desired_nodes", "max_nodes")
					},
				),
			},
			{PreConfig: func() { api.NodePoolStatusSequence("*", "running") }, Config: poolHCL(cid, "auto", raised), PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

func TestNodePoolResource_RemovingTheBudgetSendsExplicitZerosBecauseOmissionKeepsTheStoredValue(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "auto", autoscalingBudget)},
			{
				Config: poolHCL(cid, "auto"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "min_vcpus", "0"),
					resource.TestCheckResourceAttr(poolResource, "max_vcpus", "0"),
					resource.TestCheckResourceAttr(poolResource, "min_memory_gb", "0"),
					resource.TestCheckResourceAttr(poolResource, "max_memory_gb", "0"),
					func(*terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, onePoolRE)
						if len(puts) != 1 {
							return fmt.Errorf("want one PUT, got %d", len(puts))
						}
						body := bodyOf(t, puts[0])
						for _, k := range []string{"min_vcpus", "max_vcpus", "min_memory_gb", "max_memory_gb"} {
							if err := expectNumber(body, k, 0); err != nil {
								return err
							}
						}
						return forbidKeys(body, "name", "vcpus", "memory_gb", "disk_gb", "node_labels", "node_taints")
					},
				),
			},
			{Config: poolHCL(cid, "auto"), PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

// desired_nodes is inert on an autoscaling pool: the apply must not wait for
// current_nodes to reach it, and the autoscaler moving current_nodes is not drift.
func TestNodePoolResource_AutoscalingNodeCountChangeDoesNotWaitAndCurrentNodesDriftIsNotAPlan(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "auto", autoscalingBudget, "desired_nodes = 1")},
			{
				Config: poolHCL(cid, "auto", autoscalingBudget, "desired_nodes = 2"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "desired_nodes", "2"),
					// the platform never scales an autoscaling pool to desired_nodes
					resource.TestCheckResourceAttr(poolResource, "current_nodes", "1"),
				),
			},
			{
				PreConfig: func() { api.SetNodePoolCurrentNodes("*", 4) },
				Config:    poolHCL(cid, "auto", autoscalingBudget, "desired_nodes = 2"),
				PlanOnly:  true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// M-3 (review round 1): a node-count change AND a budget change in ONE apply
// must decide "wait or not" from the PLANNED budget, not the (still-fixed)
// prior: countsChanged is true here, but the pool is autoscaling as of this
// PUT, so the update must return promptly instead of waiting for
// current_nodes == desired_nodes (which the platform will never do for an
// autoscaling pool). A bounded elapsed time is the proof: waiting would run
// into the 20s update timeout, since current_nodes never moves in the fake
// for an autoscaling pool.
func TestNodePoolResource_CombinedNodeCountAndBudgetChangeUsesThePlannedBudgetAndDoesNotWait(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	started := time.Now()
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "workers", "desired_nodes = 1")},
			{
				Config: poolHCL(cid, "workers", "desired_nodes = 3", autoscalingBudget),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(poolResource, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "desired_nodes", "3"),
					resource.TestCheckResourceAttr(poolResource, "max_vcpus", "8"),
					// the platform never scales an autoscaling pool to desired_nodes;
					// a resource that waited for it would also never see this because
					// the fake's autoscaling pools never move current_nodes on their own
					resource.TestCheckResourceAttr(poolResource, "current_nodes", "1"),
					func(*terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, onePoolRE)
						if len(puts) != 1 {
							return fmt.Errorf("want one PUT, got %d", len(puts))
						}
						body := bodyOf(t, puts[0])
						for k, want := range map[string]float64{
							"min_nodes": 3, "desired_nodes": 3, "max_nodes": 3,
							"min_vcpus": 2, "max_vcpus": 8, "min_memory_gb": 4, "max_memory_gb": 16,
						} {
							if err := expectNumber(body, k, want); err != nil {
								return err
							}
						}
						return nil
					},
				),
			},
		},
	})
	if d := time.Since(started); d > 15*time.Second {
		t.Errorf("the combined node-count and budget change took %s, want well under the 20s update timeout (it must not wait once the planned budget makes the pool autoscaling)", d)
	}
}

// ---------------------------------------------------------------- 5

func TestNodePoolResource_NameSizeLabelsTaintsAndClusterChangesEachReplaceAndNeverPut(t *testing.T) {
	type variant struct {
		name, cluster    string
		vcpus, mem, disk int
		extra            []string
	}
	base := variant{name: "workers", cluster: "A", vcpus: 2, mem: 4, disk: 20}
	with := func(f func(v *variant)) variant { v := base; f(&v); return v }
	cases := map[string]struct{ from, to variant }{
		"name":       {base, with(func(v *variant) { v.name = "workers2" })},
		"vcpus":      {base, with(func(v *variant) { v.vcpus = 4 })},
		"memory_gb":  {base, with(func(v *variant) { v.mem = 8 })},
		"disk_gb":    {base, with(func(v *variant) { v.disk = 40 })},
		"cluster_id": {base, with(func(v *variant) { v.cluster = "B" })},
		"node_labels": {
			with(func(v *variant) { v.extra = []string{`node_labels = { tier = "a" }`} }),
			with(func(v *variant) { v.extra = []string{`node_labels = { tier = "b" }`} }),
		},
		"node_labels added": {base, with(func(v *variant) { v.extra = []string{`node_labels = { tier = "a" }`} })},
		"node_taints": {
			with(func(v *variant) {
				v.extra = []string{`node_taints = [{ key = "k", value = "1", effect = "PreferNoSchedule" }]`}
			}),
			with(func(v *variant) {
				v.extra = []string{`node_taints = [{ key = "k", value = "2", effect = "PreferNoSchedule" }]`}
			}),
		},
	}
	for field, c := range cases {
		t.Run(field, func(t *testing.T) {
			api := setupFake(t)
			clusters := map[string]string{"A": seedCluster(t, api, "a"), "B": seedCluster(t, api, "b")}
			render := func(v variant) string {
				return `provider "aether" {}` + poolBlock("p", clusters[v.cluster], v.name, v.vcpus, v.mem, v.disk, v.extra...)
			}
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{
					{Config: render(c.from)},
					{
						Config: render(c.to),
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(poolResource, plancheck.ResourceActionReplace),
						}},
						Check: func(*terraform.State) error {
							// a rename destroys every node on the platform and a size change
							// never reaches a machine: the provider must replace, never PUT
							if puts := requestsMatching(api, http.MethodPut, onePoolRE); len(puts) != 0 {
								return fmt.Errorf("changing %s must not PUT, got %d PUT(s): %s", field, len(puts), puts[0].Body)
							}
							if n := len(requestsMatching(api, http.MethodPost, poolsRE)); n != 2 {
								return fmt.Errorf("want the replacement to POST a second pool, got %d POST(s)", n)
							}
							if n := len(requestsMatching(api, http.MethodDelete, onePoolRE)); n != 1 {
								return fmt.Errorf("want one DELETE of the old pool, got %d", n)
							}
							return nil
						},
					},
				},
			})
		})
	}
}

// ---------------------------------------------------------------- 6

func TestNodePoolResource_DrainTimeoutIsOmittedUntilSetThenPutAndCannotBeRemoved(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: poolHCL(cid, "workers"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr(poolResource, "drain_timeout_seconds"),
					func(*terraform.State) error {
						posts := requestsMatching(api, http.MethodPost, poolsRE)
						if len(posts) != 1 {
							return fmt.Errorf("want one create, got %d", len(posts))
						}
						// asserted on the recorded bytes: null would still "carry" the key
						if strings.Contains(string(posts[0].Body), "drain_timeout_seconds") {
							return fmt.Errorf("create body must omit drain_timeout_seconds entirely: %s", posts[0].Body)
						}
						return nil
					},
				),
			},
			{
				Config: poolHCL(cid, "workers", "drain_timeout_seconds = 120"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "drain_timeout_seconds", "120"),
					func(*terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, onePoolRE)
						if len(puts) != 1 {
							return fmt.Errorf("want one PUT, got %d", len(puts))
						}
						body := bodyOf(t, puts[0])
						if got := strings.Join(keysOf(body), ","); got != "drain_timeout_seconds" {
							return fmt.Errorf("PUT keys = %s, want only drain_timeout_seconds: %s", got, puts[0].Body)
						}
						return expectNumber(body, "drain_timeout_seconds", 120)
					},
				),
			},
			{
				// The API can never clear it (omitted and null both keep the stored
				// value), so removing it is refused at plan time, never a silent replace.
				Config:      poolHCL(cid, "workers"),
				ExpectError: regexp.MustCompile(`(?is)drain_timeout_seconds.*cannot be removed`),
			},
		},
	})
	if puts := requestsMatching(api, http.MethodPut, onePoolRE); len(puts) != 1 {
		t.Fatalf("the refused removal must not reach the API, got %d PUT(s) in total", len(puts))
	}
	if dels := requestsMatching(api, http.MethodDelete, onePoolRE); len(dels) != 1 {
		// exactly one: the harness' final destroy; a silent replace would add another
		t.Fatalf("want only the final destroy's DELETE, got %d", len(dels))
	}
}

func TestNodePoolResource_DrainTimeoutOutsideTheApisWindowIsRefusedAtValidation(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	for _, v := range []int{10, 4000} {
		resource.UnitTest(t, resource.TestCase{
			IsUnitTest:               true,
			ProtoV6ProviderFactories: factories(),
			Steps: []resource.TestStep{{
				Config:      poolHCL(cid, "workers", fmt.Sprintf("drain_timeout_seconds = %d", v)),
				ExpectError: regexp.MustCompile(`(?is)30\W+(and|to)\W+3600|30\.\.3600|30-3600`),
			}},
		})
	}
	if posts := requestsMatching(api, http.MethodPost, poolsRE); len(posts) != 0 {
		t.Fatalf("a value refused at validation must not reach the API, got %d POST(s)", len(posts))
	}
}

// I-1 (review round 1): the drain-removal refusal's only documented escape
// hatch is `-replace`, and that command hits the SAME refusal - Terraform
// core's first PlanResourceChange call for `-replace` carries the REAL prior
// state, and an error there returns before the null-prior replan ever runs.
// Only a TAINTED object is planned with a null prior, so `terraform taint`
// (or `tofu taint`) is the one remedy that actually works. Both halves are
// pinned together: the refusal text must name taint (RED against the current
// `-replace` wording), and tainting must really let the removal through.
func TestNodePoolResource_DrainTimeoutRemovalRefusalPointsAtTaintAndTaintingWorks(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	withDrain := poolHCL(cid, "workers", "drain_timeout_seconds = 300")
	withoutDrain := poolHCL(cid, "workers")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: withDrain},
			{
				// a plain apply removing the attribute (no taint): refused, and the
				// message must point the practitioner at the remedy that actually works
				Config:      withoutDrain,
				ExpectError: regexp.MustCompile(`(?is)terraform\s+taint|tofu\s+taint`),
			},
			{
				// the documented remedy, applied for real: taint, then remove the
				// attribute. One DELETE of the old pool, one new POST carrying no
				// drain_timeout_seconds, and state reads null for it afterwards.
				Taint:  []string{poolResource},
				Config: withoutDrain,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr(poolResource, "drain_timeout_seconds"),
					func(*terraform.State) error {
						if n := len(requestsMatching(api, http.MethodDelete, onePoolRE)); n != 1 {
							return fmt.Errorf("want one DELETE from tainting the old pool, got %d", n)
						}
						posts := requestsMatching(api, http.MethodPost, poolsRE)
						if len(posts) != 2 {
							return fmt.Errorf("want the original create plus one new POST from the taint, got %d", len(posts))
						}
						return forbidKeys(bodyOf(t, posts[len(posts)-1]), "drain_timeout_seconds")
					},
				),
			},
		},
	})
}

// M-1 (review round 1): the drain refusal's replacement detection was
// untested. Dropping drain_timeout_seconds together with a REPLACING
// attribute change (here: a rename) must be a normal replace, never the
// removal refusal - the new pool never carries the setting either way, so
// refusing would only block a change that was already going to replace it.
func TestNodePoolResource_RenameAndDrainRemovalTogetherReplacesWithoutRefusal(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "workers", "drain_timeout_seconds = 300")},
			{
				Config: poolHCL(cid, "workers2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(poolResource, plancheck.ResourceActionReplace),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr(poolResource, "drain_timeout_seconds"),
					func(*terraform.State) error {
						body, err := lastBody(t, api, http.MethodPost, poolsRE)
						if err != nil {
							return err
						}
						return forbidKeys(body, "drain_timeout_seconds")
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 7

func TestNodePoolResource_RemovingAScaleDownKnobKeepsTheStoredValueAndPlansEmpty(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: poolHCL(cid, "workers", "scale_down_delay_seconds = 300"),
				Check:  resource.TestCheckResourceAttr(poolResource, "scale_down_delay_seconds", "300"),
			},
			{
				// The API cannot clear it, so the provider keeps what is stored.
				Config: poolHCL(cid, "workers"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "scale_down_delay_seconds", "300"),
					func(*terraform.State) error {
						if puts := requestsMatching(api, http.MethodPut, onePoolRE); len(puts) != 0 {
							return fmt.Errorf("removing the knob must not PUT, got %d", len(puts))
						}
						return nil
					},
				),
			},
			{Config: poolHCL(cid, "workers"), PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

// ---------------------------------------------------------------- 8

func TestNodePoolResource_TaintWithoutValueReadsBackEmptyAndPlansEmpty(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	// NoSchedule needs another schedulable pool in the cluster (400 no_schedulable_pool)
	cfg := `provider "aether" {}` +
		poolBlock("base", cid, "base", 2, 4, 20) +
		poolBlock("p", cid, "tainted", 2, 4, 20, `depends_on = [aether_node_pool.base]`,
			`node_taints = [{ key = "dedicated", effect = "NoSchedule" }]`)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "node_taints.#", "1"),
					resource.TestCheckResourceAttr(poolResource, "node_taints.0.key", "dedicated"),
					resource.TestCheckResourceAttr(poolResource, "node_taints.0.value", ""),
					resource.TestCheckResourceAttr(poolResource, "node_taints.0.effect", "NoSchedule"),
				),
			},
			{Config: cfg, PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

func TestNodePoolResource_TaintEffectOutsideTheThreeIsRefusedAtValidation(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      poolHCL(cid, "workers", `node_taints = [{ key = "k", effect = "Sometimes" }]`),
			ExpectError: regexp.MustCompile(`(?is)PreferNoSchedule`), // the OneOf validator lists the legal effects
		}},
	})
	if posts := requestsMatching(api, http.MethodPost, poolsRE); len(posts) != 0 {
		t.Fatalf("a bad effect must not reach the API, got %d POST(s)", len(posts))
	}
}

func TestNodePoolResource_ExplicitEmptyLabelsStayEmptyAndPlanEmpty(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	cfg := poolHCL(cid, "workers", `node_labels = {}`)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg, Check: resource.TestCheckResourceAttr(poolResource, "node_labels.%", "0")},
			{Config: cfg, PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

// M-2 (review round 1): node_labels = {} / node_taints = [] in config against
// a NULL prior must never replace the pool - mapContentChanged/
// listContentChanged already treat "both sides zero-length" as no change, but
// that null-to-empty transition itself was untested. A real ImportState
// establishes the null prior (a freshly imported pool that never had either
// attribute set), rather than relying on whatever a plain create happens to
// leave in state.
func TestNodePoolResource_EmptyLabelsAndTaintsAgainstAnImportedNullPriorDoNotReplace(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	base := poolHCL(cid, "workers")
	empty := poolHCL(cid, "workers", `node_labels = {}`, `node_taints = []`)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: base, Check: resource.ComposeTestCheckFunc(
				resource.TestCheckNoResourceAttr(poolResource, "node_labels.%"),
				resource.TestCheckNoResourceAttr(poolResource, "node_taints.#"),
			)},
			{
				ResourceName: poolResource, ImportState: true, ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[poolResource]
					if rs == nil {
						return "", fmt.Errorf("%s is not in state", poolResource)
					}
					return rs.Primary.Attributes["cluster_id"] + "/" + rs.Primary.ID, nil
				},
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
			{
				Config: empty,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(poolResource, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(poolResource, "node_labels.%", "0"),
					resource.TestCheckResourceAttr(poolResource, "node_taints.#", "0"),
					func(*terraform.State) error {
						if n := len(requestsMatching(api, http.MethodDelete, onePoolRE)); n != 0 {
							return fmt.Errorf("the null-to-empty move must never replace the pool, got %d DELETE(s)", n)
						}
						if n := len(requestsMatching(api, http.MethodPut, onePoolRE)); n != 0 {
							return fmt.Errorf("labels/taints are immutable and must never be PUT, got %d PUT(s)", n)
						}
						return nil
					},
				),
			},
			{Config: empty, PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

// ---------------------------------------------------------------- 9

func TestNodePoolResource_DeleteWaitsThroughDeletingUntil404(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.NodePoolDeleteBecomes404After("*", 2)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "workers")},
			{
				// a rename replaces; the old pool holds its name while "deleting", so
				// the create can only succeed if the destroy waited for the 404
				Config: poolHCL(cid, "workers2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(poolResource, plancheck.ResourceActionReplace),
				}},
				Check: func(*terraform.State) error {
					dels := requestsMatching(api, http.MethodDelete, onePoolRE)
					if len(dels) != 1 {
						return fmt.Errorf("want one DELETE, got %d", len(dels))
					}
					seen, afterDelete := 0, false
					for _, r := range api.Requests() {
						if r.Method == http.MethodDelete && r.Path == dels[0].Path {
							afterDelete = true
							continue
						}
						if afterDelete && r.Method == http.MethodGet && r.Path == dels[0].Path {
							seen++
						}
					}
					if seen < 3 {
						return fmt.Errorf("delete returned after %d GET(s) of the old pool, want >= 3 (2 x 200 deleting, then 404)", seen)
					}
					return nil
				},
			},
		},
	})
}

func TestNodePoolResource_DeleteSurfacesAForbiddenProbePromptly(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	cfg := func(name string) string { return poolHCL(cid, name, `timeouts = { create = "20s", delete = "30s" }`) }
	var started time.Time
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg("workers")},
			{
				PreConfig: func() {
					api.NodePoolGetFailsAfterDelete("*", http.StatusForbidden, "token revoked mid-delete")
					started = time.Now()
				},
				Config:      cfg("workers2"),
				ExpectError: loose("token revoked mid-delete"),
			},
			{
				PreConfig: func() {
					if d := time.Since(started); d > 20*time.Second {
						t.Errorf("the 403 took %s to surface; it must be reported on the first probe, not after the 30s delete timeout", d)
					}
					api.NodePoolGetFailsAfterDelete("*", 0, "")
				},
				Config: cfg("workers2"),
			},
		},
	})
}

func TestNodePoolResource_DeleteAnswered404IsSuccess(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "workers")},
			{
				PreConfig: func() { api.NodePoolDeleteRespondsNotFound(true) },
				Config:    poolHCL(cid, "workers2"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(poolResource, plancheck.ResourceActionReplace),
				}},
				Check: func(*terraform.State) error {
					if n := len(requestsMatching(api, http.MethodDelete, onePoolRE)); n != 1 {
						return fmt.Errorf("want the 404 DELETE to have been sent once, got %d", n)
					}
					return nil
				},
			},
			{PreConfig: func() { api.NodePoolDeleteRespondsNotFound(false) }, Config: poolHCL(cid, "workers2"), PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

// ---------------------------------------------------------------- 10

func TestNodePoolResource_ImportByClusterAndPoolID(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: poolHCL(cid, "workers", "desired_nodes = 2")},
			{
				ResourceName: poolResource, ImportState: true, ImportStateVerify: true,
				ImportStateIdFunc: func(s *terraform.State) (string, error) {
					rs := s.RootModule().Resources[poolResource]
					if rs == nil {
						return "", fmt.Errorf("%s is not in state", poolResource)
					}
					return rs.Primary.Attributes["cluster_id"] + "/" + rs.Primary.ID, nil
				},
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
		},
	})
}

func TestNodePoolResource_ImportWithAMalformedIDIsRefusedBeforeAnyAPICall(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	good := "22222222-2222-4222-8222-222222222222"
	steps := []resource.TestStep{{Config: poolHCL(cid, "workers")}}
	for _, id := range []string{"abc", good, good + "/notauuid", "notauuid/" + good, good + "/" + good + "/" + good} {
		steps = append(steps, resource.TestStep{
			ResourceName: poolResource, ImportState: true, ImportStateId: id,
			ExpectError: regexp.MustCompile(`(?is)(cluster_id\W*/\W*pool_id|UUID)`),
		})
	}
	before := 0
	steps[1].PreConfig = func() { before = len(api.Requests()) }
	resource.UnitTest(t, resource.TestCase{IsUnitTest: true, ProtoV6ProviderFactories: factories(), Steps: steps})
	// nothing after the create step may have looked up a made-up id
	for _, r := range api.Requests()[before:] {
		if strings.Contains(r.Path, "notauuid") || strings.HasSuffix(r.Path, "/abc") || strings.Contains(r.Path, good) {
			t.Fatalf("a malformed import id reached the API: %s %s", r.Method, r.Path)
		}
	}
}

// ---------------------------------------------------------------- 11

func TestNodePoolResource_CreateWithUnusableBodyFailsAndNeverRecordsAZeroID(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	api.NodePoolCreateRespondsWithNonJSON(true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      poolHCL(cid, "workers"),
			ExpectError: regexp.MustCompile(`(?is)(no\W+usable\W+body|not\W+JSON)`),
		}},
	})
	for _, r := range api.Requests() {
		if strings.Contains(r.Path, zeroUUID) {
			t.Fatalf("a request addressed the zero UUID: %s %s (zero id persisted to state)", r.Method, r.Path)
		}
	}
}

func TestNodePoolResource_DuplicateNameSurfacesThe409(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	seedPool(t, api, cid, "workers")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps:                    []resource.TestStep{{Config: poolHCL(cid, "workers"), ExpectError: loose("already exists")}},
	})
}

// ---------------------------------------------------------------- 12

func TestNodePoolResource_PoolDeletedOutOfBandIsRecreated(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	var firstID string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: poolHCL(cid, "workers"),
				Check: func(*terraform.State) error {
					ids := api.NodePoolIDs(cid)
					if len(ids) != 1 {
						return fmt.Errorf("want 1 pool, got %v", ids)
					}
					firstID = ids[0]
					return nil
				},
			},
			{
				PreConfig: func() { api.RemoveNodePool("*") },
				Config:    poolHCL(cid, "workers"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(poolResource, plancheck.ResourceActionCreate),
				}},
				Check: func(*terraform.State) error {
					ids := api.NodePoolIDs(cid)
					if len(ids) != 1 || ids[0] == firstID {
						return fmt.Errorf("want a NEW pool (was %s), got %v", firstID, ids)
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- extra

// A timeouts diagnostic returns before any API call.
func TestNodePoolResource_InvalidTimeoutsAreRefusedBeforeAnyAPICall(t *testing.T) {
	api := setupFake(t)
	cid := seedCluster(t, api, "c")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      poolHCL(cid, "workers", `timeouts = { create = "banana" }`),
			ExpectError: regexp.MustCompile(`(?is)duration|timeout`),
		}},
	})
	if posts := requestsMatching(api, http.MethodPost, poolsRE); len(posts) != 0 {
		t.Fatalf("invalid timeouts must be refused before the API is called, got %d POST(s)", len(posts))
	}
}
