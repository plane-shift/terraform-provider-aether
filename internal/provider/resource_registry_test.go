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

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive aether_registry (internal/provider/resource_registry.go,
// registered in provider.go's Resources() as NewRegistryResource) through a
// real terraform (or tofu) binary against the in-process fake
// (fakeapi/registry.go), under the controller rulings T11-0..T11-16 of
// task-11-brief-amended.md. testOrg's slug is "acme" (setupFake), so every
// expected harbor_project/endpoint/robot_username below is literal
// "aether-acme"/"robot$aether-acme+org-acme". Symbols the coder provides:
// NewRegistryResource, NewRegistryRobotAccountResource,
// NewRegistryCleanupPolicyResource, registered in provider.go.
//
// Until the coder implements them, EVERY test in this file and its two
// siblings (resource_registry_robot_account_test.go,
// resource_registry_cleanup_policy_test.go) fails at plan time with
// terraform-plugin-framework's own "Invalid Resource Type" diagnostic - there
// is no resource of that type name in the provider's schema yet. That is the
// expected RED state; once the resource exists the same failure mode cannot
// recur (a next PLAN would fail differently - on a real assertion - if the
// resource were implemented wrong), which is how each test below is shown
// able to fail on a wrong implementation, not just a missing one.

const (
	registryResource = "aether_registry.r"
	registryOneRE    = `/registry$`
)

// registryHCL renders the aether_registry under test; extra lines go inside
// the block (there are no Required attributes, so the block can be empty).
func registryHCL(extra ...string) string {
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_registry" "r" {
%s
}
`, strings.Join(extra, "\n"))
}

// rawCall issues a real HTTP request straight at the fake, bypassing
// Terraform entirely - used to put the fake into a state ANOTHER caller (a
// prior terraform run under a different state, the portal, a teammate's
// token) could have left it in, which is exactly what R3/R4/R5/R8 need to
// seed and no fakeapi knob models (the registry subtree's own routes already
// do the job; a caller only needs to reach them out of band).
func rawCall(t *testing.T, api *fakeapi.Server, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, api.URL()+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+api.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func rawDecode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %q: %v", b, err)
	}
	return m
}

// rawEnableRegistry POSTs .../registry directly against the fake (not
// through Terraform) and fails the test if it doesn't 201.
func rawEnableRegistry(t *testing.T, api *fakeapi.Server, org string) map[string]any {
	t.Helper()
	resp, b := rawCall(t, api, http.MethodPost, "/organizations/"+org+"/registry", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("raw enable = %d %s, want 201", resp.StatusCode, b)
	}
	return rawDecode(t, b)
}

// filterReqs narrows a request slice to one method+path-regexp - the "since a
// marker" analogue of requestsMatching (which scans the WHOLE history),
// shared by all three registry resource test files.
func filterReqs(reqs []fakeapi.Request, method, pathRE string) []fakeapi.Request {
	re := regexp.MustCompile(pathRE)
	var out []fakeapi.Request
	for _, r := range reqs {
		if r.Method == method && re.MatchString(r.Path) {
			out = append(out, r)
		}
	}
	return out
}

// writesSince is requestsSince narrowed to non-GET requests - refreshes
// (GET) are expected on every step; only a write proves an unwanted or
// missing API call.
func writesSince(api *fakeapi.Server, n int) []fakeapi.Request {
	var out []fakeapi.Request
	for _, r := range requestsSince(api, n) {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// setValues collects a Set attribute's element values from a flatmap -
// ImportStateCheck sees only the raw instance state, whose Set elements are
// keyed by an internal hashcode (not index 0,1,...), so this is the only way
// to assert Set CONTENTS there. Sorted for a deterministic comparison; the
// ".#" count key is excluded.
func setValues(attrs map[string]string, prefix string) []string {
	var out []string
	for k, v := range attrs {
		if k == prefix+".#" || !strings.HasPrefix(k, prefix+".") {
			continue
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- R1

// Fails if Create sends more than the one bodyless POST, if any field from
// the 201 (including the sensitive robot_username/robot_secret) is missing
// from state, or if purge_on_destroy does not default to false.
func TestRegistryResource_CreateSendsOnePOSTAndFullState(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: registryHCL(),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(registryResource, "purge_on_destroy", "false"),
				resource.TestCheckResourceAttr(registryResource, "enabled", "true"),
				resource.TestCheckResourceAttr(registryResource, "harbor_project", "aether-acme"),
				resource.TestCheckResourceAttr(registryResource, "quota_gb", "50"),
				resource.TestMatchResourceAttr(registryResource, "endpoint", regexp.MustCompile(`aether-acme$`)),
				resource.TestCheckResourceAttrSet(registryResource, "id"),
				resource.TestCheckResourceAttrSet(registryResource, "created_at"),
				resource.TestCheckResourceAttrSet(registryResource, "updated_at"),
				resource.TestCheckResourceAttr(registryResource, "robot_username", "robot$aether-acme+org-acme"),
				resource.TestCheckResourceAttrSet(registryResource, "robot_secret"),
				func(*terraform.State) error {
					posts := requestsMatching(api, http.MethodPost, registryOneRE)
					if len(posts) != 1 {
						return fmt.Errorf("want exactly one POST, got %d: %v", len(posts), posts)
					}
					if len(posts[0].Body) != 0 {
						return fmt.Errorf("POST body = %q, want empty (no body is read, §3)", posts[0].Body)
					}
					if g := requestsMatching(api, http.MethodGet, registryOneRE); len(g) != 0 {
						return fmt.Errorf("a plain create also GET, want zero: %v", g)
					}
					if d := requestsMatching(api, http.MethodDelete, registryOneRE); len(d) != 0 {
						return fmt.Errorf("a plain create also DELETEd, want zero: %v", d)
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- R2

// Fails if a second, unchanged apply sends anything but a refresh GET (a
// stray POST/PUT/DELETE), or if it fails to keep the write-once
// robot_secret/robot_username from the first apply (the API never returns
// them again).
func TestRegistryResource_SecondApplyIsRefreshOnlyAndKeepsCredentials(t *testing.T) {
	api := setupFake(t)
	var firstSecret, firstUser string
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: registryHCL(),
				Check: func(s *terraform.State) error {
					a := attrs(s, registryResource)
					firstSecret, firstUser = a["robot_secret"], a["robot_username"]
					return nil
				},
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    registryHCL(),
				Check: resource.ComposeTestCheckFunc(
					func(s *terraform.State) error {
						a := attrs(s, registryResource)
						if a["robot_secret"] != firstSecret {
							return fmt.Errorf("robot_secret = %q, want the first apply's %q (write-once, never re-minted)", a["robot_secret"], firstSecret)
						}
						if a["robot_username"] != firstUser {
							return fmt.Errorf("robot_username = %q, want the first apply's %q (write-once, never re-minted)", a["robot_username"], firstUser)
						}
						return nil
					},
					func(*terraform.State) error {
						if w := writesSince(api, seen); len(w) != 0 {
							return fmt.Errorf("an unchanged second apply wrote: %v, want zero writes (refresh GET only)", w)
						}
						if g := filterReqs(requestsSince(api, seen), http.MethodGet, registryOneRE); len(g) == 0 {
							return fmt.Errorf("no refresh GET was made on the second apply")
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- R3

// Fails if a 409 on create (the registry is already enabled by another
// caller) is surfaced as an error instead of adopting the existing registry,
// if adoption re-reads via anything but exactly one GET after the one POST,
// or if robot_username/robot_secret are anything but null after an adopt
// (the enable call that minted them was not this run's).
func TestRegistryResource_CreateConflictWhileEnabledAdopts(t *testing.T) {
	api := setupFake(t)
	rawEnableRegistry(t, api, testOrg)
	seen := len(api.Requests())
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: registryHCL(),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(registryResource, "enabled", "true"),
				resource.TestCheckResourceAttr(registryResource, "harbor_project", "aether-acme"),
				resource.TestCheckNoResourceAttr(registryResource, "robot_username"),
				resource.TestCheckNoResourceAttr(registryResource, "robot_secret"),
				func(*terraform.State) error {
					posts := filterReqs(requestsSince(api, seen), http.MethodPost, registryOneRE)
					gets := filterReqs(requestsSince(api, seen), http.MethodGet, registryOneRE)
					if len(posts) != 1 || len(gets) != 1 {
						return fmt.Errorf("writes since the raw enable = %d POST / %d GET, want exactly 1 each (T11-1's 409-then-GET adopt)", len(posts), len(gets))
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- R4

// Fails if a 409 reached while GET still reports the registry disabled (a
// leaked Harbor robot or a user robot literally named "org-<slug>") is
// silently adopted instead of raising an error, or if anything gets saved
// into state on that path (T11-1, aether-api#312).
func TestRegistryResource_CreateConflictWhileDisabledErrorsWithNothingSaved(t *testing.T) {
	api := setupFake(t)
	api.SeedRobot(testOrg, "org-acme", []string{"repository:push"})
	seen := len(api.Requests())
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config:      registryHCL(),
				ExpectError: loose("registry as disabled but Harbor already holds its robot"),
			},
			{
				// Nothing to refresh: the failed Create must not have saved
				// anything. The config on disk (unchanged from step 1) still asks
				// for the resource, so the post-refresh plan against an empty
				// state is a CREATE - non-empty on purpose; the point here is
				// only that Check sees an empty state.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              checkAbsentFromState(registryResource),
			},
		},
	})
	posts := filterReqs(requestsSince(api, seen), http.MethodPost, registryOneRE)
	gets := filterReqs(requestsSince(api, seen), http.MethodGet, registryOneRE)
	if len(posts) != 1 || len(gets) != 1 {
		t.Fatalf("writes = %d POST / %d GET, want exactly 1 each (the 409 then the disabled-check GET)", len(posts), len(gets))
	}
}

// ---------------------------------------------------------------- R5

// Fails if destroy sends more than one DELETE, sends a purge query parameter
// by default, or leaves any robot behind (§4: disable deletes every robot of
// the org).
func TestRegistryResource_DestroyDefaultSendsPlainDeleteNoPurge(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: registryHCL()},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(registryResource),
					func(*terraform.State) error {
						d := filterReqs(requestsSince(api, seen), http.MethodDelete, registryOneRE)
						if len(d) != 1 {
							return fmt.Errorf("DELETEs = %v, want exactly one", d)
						}
						if d[0].Query != "" {
							return fmt.Errorf("DELETE query = %q, want empty (no purge param by default)", d[0].Query)
						}
						return nil
					},
				),
			},
		},
	})
	if n := api.PurgeCount(testOrg); n != 0 {
		t.Fatalf("PurgeCount = %d, want 0 (default destroy never purges)", n)
	}
	if resp, b := rawCall(t, api, http.MethodGet, "/organizations/"+testOrg+"/registry/robot-accounts", ""); resp.StatusCode != http.StatusOK || len(rawDecode(t, b)["items"].([]any)) != 0 {
		t.Fatalf("robots after destroy = %d %s, want an empty list", resp.StatusCode, b)
	}
}

// ---------------------------------------------------------------- R6

// Fails if setting purge_on_destroy in place makes any write call (it must
// be a pure state update), or if destroy with it set sends anything but
// exactly one DELETE with the literal query purge=true.
func TestRegistryResource_PurgeOnDestroyIsInPlaceThenSendsPurgeTrue(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: registryHCL()},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    registryHCL("purge_on_destroy = true"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(registryResource, "purge_on_destroy", "true"),
					func(*terraform.State) error {
						if w := writesSince(api, seen); len(w) != 0 {
							return fmt.Errorf("changing purge_on_destroy alone wrote: %v, want zero writes", w)
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: func(*terraform.State) error {
					d := filterReqs(requestsSince(api, seen), http.MethodDelete, registryOneRE)
					if len(d) != 1 || d[0].Query != "purge=true" {
						return fmt.Errorf("destroy DELETEs = %v, want exactly one with query purge=true", d)
					}
					return nil
				},
			},
		},
	})
	if n := api.PurgeCount(testOrg); n != 1 {
		t.Fatalf("PurgeCount = %d, want 1 (the literal purge=true must have counted)", n)
	}
}

// ---------------------------------------------------------------- R7

// Fails if Read does not remove a registry that went disabled out of band, or
// if the next plan does not recreate it with a fresh POST.
func TestRegistryResource_ReadDisabledRemovesAndNextPlanRecreates(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: registryHCL()},
			{
				PreConfig: func() { api.Disable(testOrg) },
				Config:    registryHCL(),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(registryResource, "enabled", "true"),
					func(*terraform.State) error {
						if posts := requestsMatching(api, http.MethodPost, registryOneRE); len(posts) != 2 {
							return fmt.Errorf("total POSTs = %d, want 2 (the original create plus the recreate)", len(posts))
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- R8

// Fails if import of an enabled registry does not adopt every GET-derived
// field, or if it invents a value for robot_username/robot_secret (the API
// never returns them on GET).
func TestRegistryResource_ImportOfAnEnabledRegistryAdoptsWithNullCredentials(t *testing.T) {
	api := setupFake(t)
	enabled := rawEnableRegistry(t, api, testOrg)
	regID := enabled["registry"].(map[string]any)["id"].(string)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName:       registryResource,
			ImportState:        true,
			ImportStatePersist: true,
			Config:             registryHCL(),
			ImportStateId:      regID,
			ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 {
					return fmt.Errorf("want exactly one imported instance, got %d", len(is))
				}
				a := is[0].Attributes
				if a["id"] != regID || a["enabled"] != "true" || a["harbor_project"] != "aether-acme" {
					t.Logf("imported attrs = %v", a)
					return fmt.Errorf("imported id/enabled/harbor_project = %q/%q/%q, want %q/true/aether-acme", a["id"], a["enabled"], a["harbor_project"], regID)
				}
				for _, k := range []string{"robot_username", "robot_secret"} {
					if v, has := a[k]; has {
						return fmt.Errorf("imported %s = %q, want null (never known from a GET-based import)", k, v)
					}
				}
				return nil
			},
		}},
	})
}

// Fails if importing while the registry is disabled is silently accepted
// instead of erroring with the measured message (T11-4).
func TestRegistryResource_ImportWhileDisabledErrors(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName:  registryResource,
			ImportState:   true,
			Config:        registryHCL(),
			ImportStateId: "00000000-0000-4000-8000-000000000000",
			ExpectError:   loose("the organization's registry is not enabled"),
		}},
	})
}

// ---------------------------------------------------------------- I-1 (T11-18)

// Fails if Read carries robot_username/robot_secret forward after the
// registry's id changes out of band (an out-of-band disable + re-enable
// mints a NEW registry row and a NEW default robot with a secret this run
// never saw - T11-18 amends T11-2: Read must null both credentials and, per
// the ruling, emit a Warning). Asserts the id picks up the new registry AND
// both credentials go null on that same refresh. v1.16.0's plugin-testing
// harness has no way to assert on a Warning diagnostic from a successful
// apply (TDD: "...cannot assert a warning" - same limitation noted for Task
// 9), so the absence of the stale credentials is the whole test; the
// id-changed Warning itself is exercised only by inspection of Read.
func TestRegistryResource_ReadAfterOutOfBandReenableNullsCredentials(t *testing.T) {
	api := setupFake(t)
	var newID string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: registryHCL()},
			{
				PreConfig: func() {
					api.Disable(testOrg)
					reenabled := rawEnableRegistry(t, api, testOrg)
					newID = reenabled["registry"].(map[string]any)["id"].(string)
				},
				Config: registryHCL(),
				Check: resource.ComposeTestCheckFunc(
					func(s *terraform.State) error {
						a := attrs(s, registryResource)
						if a["id"] != newID {
							return fmt.Errorf("id after the out-of-band re-enable = %q, want the NEW registry id %q", a["id"], newID)
						}
						return nil
					},
					resource.TestCheckNoResourceAttr(registryResource, "robot_username"),
					resource.TestCheckNoResourceAttr(registryResource, "robot_secret"),
				),
			},
		},
	})
}

// ---------------------------------------------------------------- m-3(c)/(d)

// Fails if a 409 whose code is NOT robot_exists is silently adopted instead
// of surfaced as an error: T11-1's adopt path is gated on robot_exists
// specifically (m-3(c)).
func TestRegistryResource_CreateConflictWithOtherCodeErrors(t *testing.T) {
	api := setupFake(t)
	route := "POST /organizations/" + testOrg + "/registry"
	api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
		fakeapi.WriteJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "some_other_conflict", "message": "a different conflict entirely"},
		})
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config:      registryHCL(),
				ExpectError: loose("a different conflict entirely"),
			},
			{
				// Nothing to refresh: the failed Create must not have saved
				// anything (mirrors R4's shape).
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check:              checkAbsentFromState(registryResource),
			},
		},
	})
}

// Fails if a non-204 from DELETE /registry is swallowed instead of surfaced
// as an error with state KEPT (T11-3; m-3(d)).
func TestRegistryResource_DestroyNon204Errors(t *testing.T) {
	api := setupFake(t)
	var route string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: registryHCL(),
				Check: func(s *terraform.State) error {
					route = "DELETE /organizations/" + testOrg + "/registry"
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						http.Error(w, "internal_error: something else broke", http.StatusInternalServerError)
					}
					return nil
				},
			},
			{
				Config:      providerOnlyHCL,
				ExpectError: loose("internal_error"),
			},
			{
				PreConfig: func() { delete(api.Handlers, route) },
				// The failed destroy must have kept the registry in state; a
				// post-refresh plan against a state that still has it (config
				// on disk is providerOnlyHCL) wants to destroy it again.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					if _, ok := s.RootModule().Resources[registryResource]; !ok {
						return fmt.Errorf("%s missing after the failed destroy, want it KEPT", registryResource)
					}
					return nil
				},
			},
		},
	})
}
