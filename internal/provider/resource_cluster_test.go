package provider_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive the aether_cluster resource through a real terraform (or
// tofu) binary against the in-process fake. resource.UnitTest + IsUnitTest
// means TF_ACC is not required.

func factories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"aether": providerserver.NewProtocol6WithError(provider.New("test")()),
	}
}

const testOrg = "11111111-1111-4111-8111-111111111111"

func setupFake(t *testing.T) *fakeapi.Server {
	t.Helper()
	api := fakeapi.New(t)
	api.AddOrg(testOrg, "Acme", "acme")
	t.Setenv("AETHER_ENDPOINT", api.URL())
	t.Setenv("AETHER_API_TOKEN", api.Token())
	// A developer's exported value would switch Configure onto the explicit-org
	// path, which is not what these tests exercise.
	t.Setenv("AETHER_ORGANIZATION_ID", "")
	return api
}

// clusterHCL renders the cluster under test; extra lines go inside the block.
func clusterHCL(name, region, version string, extra ...string) string {
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_cluster" "c" {
  name        = %q
  region      = %q
  k8s_version = %q
%s
}
`, name, region, version, strings.Join(extra, "\n"))
}

func clusterCfg(version string) string { return clusterHCL("prod", "eu-fi", version) }

// loose turns a phrase into a regexp that survives terraform's wrapping of
// diagnostic text at ~78 columns (any run of non-word characters, including
// the newline and the box-drawing prefix, stands for each space).
func loose(phrase string) *regexp.Regexp {
	parts := strings.Fields(phrase)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`(?is)` + strings.Join(parts, `\W+`))
}

func requestsMatching(api *fakeapi.Server, method string, pathRE string) []fakeapi.Request {
	re := regexp.MustCompile(pathRE)
	var out []fakeapi.Request
	for _, r := range api.Requests() {
		if r.Method == method && re.MatchString(r.Path) {
			out = append(out, r)
		}
	}
	return out
}

func bodyOf(t *testing.T, r fakeapi.Request) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("request body of %s %s is not a JSON object: %q", r.Method, r.Path, r.Body)
	}
	return m
}

const (
	clustersRE      = `/clusters$`
	oneClusterRE    = `/clusters/[^/]+$`
	upgradeRE       = `/clusters/[^/]+/upgrade$`
	zeroUUID        = "00000000-0000-0000-0000-000000000000"
	clusterResource = "aether_cluster.c"
)

func TestClusterResource_CreateWaitsForRunningAndTolerates137vs1370(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "provisioning", "provisioning", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: clusterCfg("1.37"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(clusterResource, "status", "running"),
				resource.TestCheckResourceAttr(clusterResource, "k8s_version", "1.37"),
				// the API stored the RESOLVED patch and reports it once running
				resource.TestCheckResourceAttr(clusterResource, "observed_kubernetes_version", "1.37.2"),
				resource.TestCheckResourceAttr(clusterResource, "public_endpoint", "false"),
				resource.TestCheckResourceAttrSet(clusterResource, "id"),
			),
		}, {
			// second plan must be empty: configured 1.37 vs the API's resolved 1.37.2 is not drift
			Config: clusterCfg("1.37"), PlanOnly: true, ExpectNonEmptyPlan: false,
		}},
	})
}

func TestClusterResource_UpgradeGoesThroughUpgradeEndpoint(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: clusterCfg("1.36")},
			{
				Config: clusterCfg("1.37"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterResource, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeTestCheckFunc(
					func(*terraform.State) error {
						ups := requestsMatching(api, http.MethodPost, upgradeRE)
						if len(ups) != 1 {
							return fmt.Errorf("want exactly one POST …/upgrade, got %d", len(ups))
						}
						var body struct {
							TargetVersion string `json:"target_version"`
						}
						if err := json.Unmarshal(ups[0].Body, &body); err != nil {
							return fmt.Errorf("upgrade body %q: %w", ups[0].Body, err)
						}
						if body.TargetVersion != "1.37" {
							return fmt.Errorf("upgrade target_version = %q, want 1.37", body.TargetVersion)
						}
						// Only k8s_version changed: nothing for PUT to carry.
						if puts := requestsMatching(api, http.MethodPut, oneClusterRE); len(puts) != 0 {
							return fmt.Errorf("a version-only change must not PUT, got %d PUT(s)", len(puts))
						}
						return nil
					},
					resource.TestCheckResourceAttr(clusterResource, "k8s_version", "1.37"),
					// The API rolled the control plane to the resolved patch; state must follow.
					resource.TestCheckResourceAttr(clusterResource, "observed_kubernetes_version", "1.37.2"),
					resource.TestCheckResourceAttr(clusterResource, "status", "running"),
				),
			},
		},
	})
}

func TestClusterResource_DowngradeIsRefusedAtPlan(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: clusterCfg("1.37")},
			{Config: clusterCfg("1.36"), ExpectError: regexp.MustCompile(`(?i)downgrade|backwards`)},
		},
	})
	if ups := requestsMatching(api, http.MethodPost, upgradeRE); len(ups) != 0 {
		t.Fatalf("a refused downgrade must not reach the API, got %d upgrade call(s)", len(ups))
	}
	if puts := requestsMatching(api, http.MethodPut, oneClusterRE); len(puts) != 0 {
		t.Fatalf("a refused downgrade must not reach the API, got %d PUT(s)", len(puts))
	}
}

func TestClusterResource_FailedStatusSurfacesMessage(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "provisioning", "failed")
	api.SetStatusMessage("*", "no capacity in eu-fi")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps:                    []resource.TestStep{{Config: clusterCfg("1.37"), ExpectError: loose("no capacity in eu-fi")}},
	})
}

func TestClusterResource_DegradedTerminalStatusSucceeds(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "provisioning", "degraded")
	api.SetStatusMessage("*", "1 of 3 control plane pods not ready")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: clusterCfg("1.37"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(clusterResource, "status", "degraded"),
				resource.TestCheckResourceAttr(clusterResource, "status_message", "1 of 3 control plane pods not ready"),
				resource.TestCheckResourceAttrSet(clusterResource, "id"),
			),
		}},
	})
}

func TestClusterResource_DeleteWaitsFor404AndRegionForcesReplace(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	api.DeleteBecomes404After("*", 2)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: clusterCfg("1.37")},
			{
				Config: clusterHCL("prod", "eu-fi2", "1.37"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterResource, plancheck.ResourceActionReplace),
				}},
				Check: func(*terraform.State) error {
					dels := requestsMatching(api, http.MethodDelete, oneClusterRE)
					if len(dels) != 1 {
						return fmt.Errorf("want one DELETE, got %d", len(dels))
					}
					// The two "deleting" reads and the 404 that ends the wait must all
					// have been polled for the OLD cluster after its DELETE.
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
						return fmt.Errorf("delete returned after %d GET(s) of the old cluster, want >= 3 (2 x deleting, then 404)", seen)
					}
					if len(requestsMatching(api, http.MethodPost, clustersRE)) != 2 {
						return fmt.Errorf("want the replacement to POST a second cluster")
					}
					return nil
				},
			},
		},
	})
}

func TestClusterResource_ImportByID(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: clusterCfg("1.37")},
			{ResourceName: clusterResource, ImportState: true, ImportStateVerify: true, // k8s_version: state keeps the configured spelling ("1.37"), the import
				// reads the API's resolved "1.37.2"; TestClusterResource_ImportedClusterFirstPlan... pins that.
				ImportStateVerifyIgnore: []string{"timeouts", "k8s_version"}},
		},
	})
}

// plan is Optional (NOT Computed) + RequiresReplace: what the config says is
// what state holds, and the API's plan_slug echo must never read as drift.
func TestClusterResource_PlanIsKeptAndChangingItReplaces(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	withPlan := func(p string) string { return clusterHCL("prod", "eu-fi", "1.37", fmt.Sprintf("plan = %q", p)) }
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: withPlan("starter"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(clusterResource, "plan", "starter"),
					func(*terraform.State) error {
						posts := requestsMatching(api, http.MethodPost, clustersRE)
						if len(posts) != 1 || bodyOf(t, posts[0])["plan"] != "starter" {
							return fmt.Errorf("create must send plan=starter, got %v", posts)
						}
						return nil
					},
				),
			},
			{Config: withPlan("starter"), PlanOnly: true, ExpectNonEmptyPlan: false},
			{
				Config: withPlan("growth"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterResource, plancheck.ResourceActionReplace),
				}},
				Check: resource.TestCheckResourceAttr(clusterResource, "plan", "growth"),
			},
		},
	})
}

// A cluster created without a plan has no plan_slug; the absent value must not
// show up as drift on the next plan, and no plan may be sent.
func TestClusterResource_PlanlessClusterHasNoPlanAndPlansEmpty(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: clusterCfg("1.37"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr(clusterResource, "plan"),
					func(*terraform.State) error {
						posts := requestsMatching(api, http.MethodPost, clustersRE)
						if len(posts) != 1 {
							return fmt.Errorf("want one create, got %d", len(posts))
						}
						if _, has := bodyOf(t, posts[0])["plan"]; has {
							return fmt.Errorf("plan must be omitted from the create body when not configured: %s", posts[0].Body)
						}
						return nil
					},
				),
			},
			{Config: clusterCfg("1.37"), PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
}

func TestClusterResource_EmptyPlanIsRefusedAtValidation(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      clusterHCL("prod", "eu-fi", "1.37", `plan = ""`),
			ExpectError: regexp.MustCompile(`(?is)plan.*(empty|at\W+least|length|blank)`),
		}},
	})
	if posts := requestsMatching(api, http.MethodPost, clustersRE); len(posts) != 0 {
		t.Fatalf("validation must refuse before any API call, got %d POST(s)", len(posts))
	}
}

// public_endpoint is immutable in the API (PUT rejects a changed value with
// 422 public_endpoint/immutable), so a change can only be a replacement.
func TestClusterResource_PublicEndpointChangeReplaces(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: clusterHCL("prod", "eu-fi", "1.37", "public_endpoint = false"),
				Check:  resource.TestCheckResourceAttr(clusterResource, "public_endpoint", "false"),
			},
			{
				Config: clusterHCL("prod", "eu-fi", "1.37", "public_endpoint = true"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterResource, plancheck.ResourceActionReplace),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(clusterResource, "public_endpoint", "true"),
					func(*terraform.State) error {
						posts := requestsMatching(api, http.MethodPost, clustersRE)
						if len(posts) != 2 || bodyOf(t, posts[1])["public_endpoint"] != true {
							return fmt.Errorf("the replacement must POST public_endpoint=true, got %v", posts)
						}
						if puts := requestsMatching(api, http.MethodPut, oneClusterRE); len(puts) != 0 {
							return fmt.Errorf("public_endpoint must never be PUT, got %d PUT(s)", len(puts))
						}
						return nil
					},
				),
			},
		},
	})
}

// A cluster deleted behind Terraform's back is a re-create, not an error.
func TestClusterResource_ClusterDeletedOutOfBandIsRecreated(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	var firstID string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: clusterCfg("1.37"),
				Check: func(*terraform.State) error {
					ids := api.ClusterIDs()
					if len(ids) != 1 {
						return fmt.Errorf("want 1 cluster, got %v", ids)
					}
					firstID = ids[0]
					return nil
				},
			},
			{
				PreConfig: func() { api.RemoveCluster("*") },
				Config:    clusterCfg("1.37"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterResource, plancheck.ResourceActionCreate),
				}},
				Check: func(*terraform.State) error {
					ids := api.ClusterIDs()
					if len(ids) != 1 || ids[0] == firstID {
						return fmt.Errorf("want a NEW cluster (was %s), got %v", firstID, ids)
					}
					return nil
				},
			},
		},
	})
}

// Renaming is the one in-place field update: PUT carries name only. public_endpoint
// and plan are immutable and must not ride along; the version has its own endpoint.
func TestClusterResource_RenameUpdatesInPlaceViaPutWithNameOnly(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: clusterHCL("prod", "eu-fi", "1.37", `plan = "starter"`, "public_endpoint = true")},
			{
				Config: clusterHCL("prod-renamed", "eu-fi", "1.37", `plan = "starter"`, "public_endpoint = true"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterResource, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(clusterResource, "name", "prod-renamed"),
					func(*terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, oneClusterRE)
						if len(puts) != 1 {
							return fmt.Errorf("want exactly one PUT, got %d", len(puts))
						}
						body := bodyOf(t, puts[0])
						if body["name"] != "prod-renamed" {
							return fmt.Errorf("PUT body = %s, want name=prod-renamed", puts[0].Body)
						}
						for _, immutable := range []string{"public_endpoint", "plan", "k8s_version"} {
							if _, has := body[immutable]; has {
								return fmt.Errorf("PUT body carries %q: %s", immutable, puts[0].Body)
							}
						}
						if ups := requestsMatching(api, http.MethodPost, upgradeRE); len(ups) != 0 {
							return fmt.Errorf("a rename must not POST …/upgrade, got %d", len(ups))
						}
						if n := len(requestsMatching(api, http.MethodPost, clustersRE)); n != 1 {
							return fmt.Errorf("a rename must not re-create the cluster, %d POST(s)", n)
						}
						return nil
					},
				),
			},
		},
	})
}

// A 202 whose body is not the Cluster JSON must fail the create — and must not
// persist the zero UUID as the cluster id (an orphaned, un-importable cluster).
func TestClusterResource_CreateWithUnusableBodyFailsAndNeverRecordsAZeroID(t *testing.T) {
	api := setupFake(t)
	api.CreateRespondsWithNonJSON(true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      clusterCfg("1.37"),
			ExpectError: regexp.MustCompile(`(?is)(no\W+usable\W+body|not\W+JSON)`),
		}},
	})
	// Had the zero id reached state, the harness' own destroy (or a wait) would
	// have addressed that cluster.
	for _, r := range api.Requests() {
		if strings.Contains(r.Path, zeroUUID) {
			t.Fatalf("a request addressed the zero UUID: %s %s (zero id persisted to state)", r.Method, r.Path)
		}
	}
}

// A 403 on the follow-up GET during delete is an error to report at once, with
// the API's own message — not a wait to the delete timeout ending in a
// timeout diagnostic.
func TestClusterResource_DeleteSurfacesAForbiddenProbePromptly(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	cfg := func(region string) string {
		return clusterHCL("prod", region, "1.37", `timeouts = { delete = "30s" }`)
	}
	var started time.Time
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg("eu-fi")},
			{
				// region forces a replace, whose destroy half hits the 403.
				PreConfig: func() {
					api.GetFailsAfterDelete("*", http.StatusForbidden, "token revoked mid-delete")
					started = time.Now()
				},
				Config:      cfg("eu-fi2"),
				ExpectError: loose("token revoked mid-delete"),
			},
			{
				// Let the harness' cleanup destroy succeed. A slow failure above
				// (the timeout path) shows here as elapsed time.
				PreConfig: func() {
					if d := time.Since(started); d > 20*time.Second {
						t.Errorf("the 403 took %s to surface; it must be reported on the first probe, not after the 30s delete timeout", d)
					}
					api.GetFailsAfterDelete("*", 0, "")
				},
				Config: cfg("eu-fi2"),
			},
		},
	})
}

// A replacement builds a brand-new cluster, so it may run an OLDER version than
// the one it replaces. ModifyPlan must refuse backwards moves only when the
// same plan updates the cluster in place (DowngradeIsRefusedAtPlan is the
// control for that). ModifyPlan hand-duplicates the list of replace-forcing
// attributes, so every one of them is exercised here.
func TestClusterResource_OlderVersionIsAllowedWhenTheClusterIsReplaced(t *testing.T) {
	cases := []struct {
		name  string
		cfg   string
		check resource.TestCheckFunc
	}{
		{"region", clusterHCL("prod", "eu-fi2", "1.36"), resource.TestCheckResourceAttr(clusterResource, "region", "eu-fi2")},
		{"public_endpoint", clusterHCL("prod", "eu-fi", "1.36", "public_endpoint = true"), resource.TestCheckResourceAttr(clusterResource, "public_endpoint", "true")},
		{"plan", clusterHCL("prod", "eu-fi", "1.36", `plan = "starter"`), resource.TestCheckResourceAttr(clusterResource, "plan", "starter")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := setupFake(t)
			api.StatusSequence("*", "running")
			api.DeleteBecomes404After("*", 0)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{
					{Config: clusterHCL("prod", "eu-fi", "1.37")},
					{
						Config: tc.cfg,
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(clusterResource, plancheck.ResourceActionReplace),
						}},
						Check: resource.ComposeTestCheckFunc(
							tc.check,
							resource.TestCheckResourceAttr(clusterResource, "k8s_version", "1.36"),
							func(*terraform.State) error {
								if ups := requestsMatching(api, http.MethodPost, upgradeRE); len(ups) != 0 {
									return fmt.Errorf("a replacement must create, not upgrade; got %d upgrade call(s)", len(ups))
								}
								if n := len(requestsMatching(api, http.MethodPost, clustersRE)); n != 2 {
									return fmt.Errorf("want 2 cluster POSTs (original + replacement), got %d", n)
								}
								return nil
							},
						),
					},
				},
			})
		})
	}
}

// getsAfterUpgradePOST counts single-cluster GETs recorded after the
// (first) upgrade POST.
func getsAfterUpgradePOST(api *fakeapi.Server) int {
	after, n := false, 0
	for _, r := range api.Requests() {
		if r.Method == http.MethodPost && regexp.MustCompile(upgradeRE).MatchString(r.Path) {
			after = true
			continue
		}
		if after && r.Method == http.MethodGet && regexp.MustCompile(oneClusterRE).MatchString(r.Path) {
			n++
		}
	}
	return n
}

// The upgrade POST answers 202 while the row is still `running` with the OLD
// observed version; status flips to upgrading later and back to running when
// the rollout is done. An apply that returns on the first `running` probe
// reports success before the control plane has moved.
func TestClusterResource_UpgradeWaitsForTheRolloutNotTheFirstRunningProbe(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	api.SetUpgradeLag(2, 1) // reads 1-2: old running, read 3: upgrading, read 4: running + new observed
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: clusterCfg("1.36"),
				Check:  resource.TestCheckResourceAttr(clusterResource, "observed_kubernetes_version", "1.36.4"),
			},
			{
				Config: clusterCfg("1.37"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(clusterResource, "observed_kubernetes_version", "1.37.2"),
					resource.TestCheckResourceAttr(clusterResource, "status", "running"),
					func(*terraform.State) error {
						if n := getsAfterUpgradePOST(api); n < 4 {
							return fmt.Errorf("only %d GET(s) after the upgrade POST; the wait must poll through upgrading to the finished rollout (>= 4)", n)
						}
						return nil
					},
				),
			},
		},
	})
}

// "1.37" resolves to 1.37.2 in the API; naming the patch the cluster already
// runs, or a higher patch of the same minor, is not a downgrade.
func TestClusterResource_BareMinorToExplicitPatchOfTheSameMinorIsNotADowngrade(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: clusterCfg("1.37"), Check: resource.TestCheckResourceAttr(clusterResource, "observed_kubernetes_version", "1.37.2")},
			{
				// pin what it already runs: must plan and apply (a no-op as far as the API is concerned)
				Config: clusterCfg("1.37.2"),
				Check: func(*terraform.State) error {
					if ups := requestsMatching(api, http.MethodPost, upgradeRE); len(ups) != 0 {
						return fmt.Errorf("pinning the patch the cluster runs must not upgrade, got %d upgrade call(s)", len(ups))
					}
					return nil
				},
			},
			{
				PreConfig: func() { api.SetLatestPatch("1.37", 5) },
				Config:    clusterCfg("1.37.5"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(clusterResource, "k8s_version", "1.37.5"),
					resource.TestCheckResourceAttr(clusterResource, "observed_kubernetes_version", "1.37.5"),
					func(*terraform.State) error {
						ups := requestsMatching(api, http.MethodPost, upgradeRE)
						if len(ups) != 1 {
							return fmt.Errorf("want exactly one upgrade POST, got %d", len(ups))
						}
						if got := bodyOf(t, ups[0])["target_version"]; got != "1.37.5" {
							return fmt.Errorf("upgrade target_version = %v, want 1.37.5", got)
						}
						return nil
					},
				),
			},
		},
	})
}

// Control for the test above: an explicit patch moved to a LOWER patch of the
// same minor is still a downgrade and is refused at plan.
func TestClusterResource_LowerPatchOfTheSameMinorIsStillADowngrade(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: clusterCfg("1.37.2")},
			{Config: clusterCfg("1.37.1"), ExpectError: regexp.MustCompile(`(?i)downgrade|backwards`)},
		},
	})
	if ups := requestsMatching(api, http.MethodPost, upgradeRE); len(ups) != 0 {
		t.Fatalf("a refused downgrade must not reach the API, got %d upgrade call(s)", len(ups))
	}
}

// An Update that sends nothing (here: only the timeouts attribute changed) has
// nothing to wait for. It must not block on, or fail with, the cluster's
// current status — an upgrading or failed cluster included.
func TestClusterResource_UpdateThatCallsNoAPIDoesNotWait(t *testing.T) {
	for _, status := range []string{"upgrading", "failed"} {
		t.Run(status, func(t *testing.T) {
			api := setupFake(t)
			api.StatusSequence("*", "running")
			cfg := func(update string) string {
				return clusterHCL("prod", "eu-fi", "1.37", fmt.Sprintf(`timeouts = { update = %q }`, update))
			}
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{
					{Config: cfg("5s")},
					{
						// The cluster is now upgrading/failed forever; the update timeout is short
						// so a regression that waits fails in seconds, not in 30 minutes.
						PreConfig: func() { api.StatusSequence("*", status) },
						Config:    cfg("6s"),
						ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
							plancheck.ExpectResourceAction(clusterResource, plancheck.ResourceActionUpdate),
						}},
						Check: func(*terraform.State) error {
							if puts := requestsMatching(api, http.MethodPut, oneClusterRE); len(puts) != 0 {
								return fmt.Errorf("a timeouts-only change must not PUT, got %d", len(puts))
							}
							if ups := requestsMatching(api, http.MethodPost, upgradeRE); len(ups) != 0 {
								return fmt.Errorf("a timeouts-only change must not upgrade, got %d", len(ups))
							}
							return nil
						},
					},
				},
			})
		})
	}
}

// KNOWN behaviour, documented in the resource docs: the first plan after
// importing a cluster is NON-empty. Import reads the API's resolved patch
// ("1.37.2") into state; the configuration says "1.37", and core rejects a
// planned value that differs from the config for a non-Computed attribute, so
// the plan is an in-place update of the version spelling (no API call needed).
func TestClusterResource_ImportedClusterFirstPlanIsNonEmptyForVersionSpelling(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				// the cluster exists only in the API; nothing in Terraform state yet
				PreConfig: func() {
					req, _ := http.NewRequest(http.MethodPost, api.URL()+"/api/v1/organizations/"+testOrg+"/clusters",
						strings.NewReader(`{"name":"prod","region":"eu-fi","k8s_version":"1.37"}`))
					req.Header.Set("Authorization", "Bearer "+api.Token())
					req.Header.Set("Content-Type", "application/json")
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					_ = resp.Body.Close()
				},
				Config:             clusterCfg("1.37"),
				ResourceName:       clusterResource,
				ImportState:        true,
				ImportStatePersist: true,
				ImportStateIdFunc:  func(*terraform.State) (string, error) { return api.ClusterIDs()[0], nil },
			},
			{Config: clusterCfg("1.37"), PlanOnly: true, ExpectNonEmptyPlan: true},
		},
	})
}
