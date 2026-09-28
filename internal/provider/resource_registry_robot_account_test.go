package provider_test

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive aether_registry_robot_account
// (internal/provider/resource_registry_robot_account.go, registered in
// provider.go's Resources() as NewRegistryRobotAccountResource) through a
// real terraform (or tofu) binary against the in-process fake
// (fakeapi/registry.go), under the controller rulings T11-6..T11-8 of
// task-11-brief-amended.md. testOrg's slug is "acme" (setupFake).
//
// Every test here fails against the current (unimplemented) provider with
// terraform-plugin-framework's "Invalid Resource Type"/"Resource Type Not
// Found" diagnostic - the expected RED state; each test's own doc comment
// says what a WRONG implementation would still get caught on.

const (
	robotAccountResource = "aether_registry_robot_account.b"
	robotsListRE         = `/registry/robot-accounts$`
	robotOneRE           = `/registry/robot-accounts/[^/]+$`
)

// robotAccountHCL renders the robot account under test; extra lines go
// inside the block. permissions is always an explicit HCL list literal.
func robotAccountHCL(name string, permissions []string, extra ...string) string {
	quoted := make([]string, len(permissions))
	for i, p := range permissions {
		quoted[i] = fmt.Sprintf("%q", p)
	}
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_registry_robot_account" "b" {
  name        = %q
  permissions = [%s]
%s
}
`, name, strings.Join(quoted, ", "), strings.Join(extra, "\n"))
}

// robotWritesSince returns "POST"/"DELETE" for every robot-account write
// since the n-th recorded request, in REQUEST order (not grouped by kind) -
// the shape TestRobotAccountResource_NameChangeReplaces checks.
func robotWritesSince(api *fakeapi.Server, n int) []string {
	postRE, oneRE := regexp.MustCompile(robotsListRE), regexp.MustCompile(robotOneRE)
	var out []string
	for _, r := range requestsSince(api, n) {
		switch {
		case r.Method == http.MethodPost && postRE.MatchString(r.Path):
			out = append(out, "POST")
		case r.Method == http.MethodDelete && oneRE.MatchString(r.Path):
			out = append(out, "DELETE")
		}
	}
	return out
}

// ---------------------------------------------------------------- B1

// Fails if Create sends anything but {"name":…,"permissions":[SORTED]}, or if
// harbor_username/secret/harbor_id are missing or wrong from the 201 (§6).
func TestRobotAccountResource_CreateSendsSortedPermissionsExactBody(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			// Unsorted on purpose ("push" > "pull" alphabetically).
			Config: robotAccountHCL("ci", []string{"push", "pull"}),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(robotAccountResource, "name", "ci"),
				resource.TestCheckResourceAttr(robotAccountResource, "permissions.#", "2"),
				resource.TestCheckResourceAttr(robotAccountResource, "harbor_username", "robot$aether-acme+ci"),
				resource.TestCheckResourceAttrSet(robotAccountResource, "secret"),
				resource.TestCheckResourceAttrSet(robotAccountResource, "harbor_id"),
				resource.TestCheckResourceAttrSet(robotAccountResource, "created_at"),
				resource.TestCheckResourceAttrSet(robotAccountResource, "updated_at"),
				func(*terraform.State) error {
					posts := requestsMatching(api, http.MethodPost, robotsListRE)
					if len(posts) != 1 {
						return fmt.Errorf("want exactly one POST, got %d: %v", len(posts), posts)
					}
					if got, want := compactJSON(t, posts[0].Body), `{"name":"ci","permissions":["pull","push"]}`; got != want {
						return fmt.Errorf("POST body = %s, want exactly %s (sorted)", got, want)
					}
					if d := requestsMatching(api, http.MethodDelete, robotOneRE); len(d) != 0 {
						return fmt.Errorf("a plain create also DELETEd: %v", d)
					}
					if g := requestsMatching(api, http.MethodGet, robotsListRE); len(g) != 0 {
						return fmt.Errorf("a plain create also read the list: %v", g)
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- B2

// Fails if changing permissions does anything but a full replace (DELETE the
// old id, POST a new one) - there is no update route (§6/§8).
func TestRobotAccountResource_PermissionsChangeReplaces(t *testing.T) {
	api := setupFake(t)
	var oldID string
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: robotAccountHCL("ci", []string{"push"}),
				Check: func(s *terraform.State) error {
					oldID = attrs(s, robotAccountResource)["id"]
					return nil
				},
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    robotAccountHCL("ci", []string{"push", "pull"}),
				Check: func(s *terraform.State) error {
					newID := attrs(s, robotAccountResource)["id"]
					if newID == "" || newID == oldID {
						return fmt.Errorf("id after the permissions change = %q (was %q), want a NEW id from a replacement", newID, oldID)
					}
					deletes := filterReqs(requestsSince(api, seen), http.MethodDelete, robotOneRE)
					if len(deletes) != 1 || !strings.HasSuffix(deletes[0].Path, "/"+oldID) {
						return fmt.Errorf("DELETEs = %v, want exactly one targeting the old id %s", deletes, oldID)
					}
					posts := filterReqs(requestsSince(api, seen), http.MethodPost, robotsListRE)
					if len(posts) != 1 {
						return fmt.Errorf("POSTs = %v, want exactly one (the replacement)", posts)
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- B3

// Fails if changing name does anything but a full replace (§6:
// RequiresReplace on name).
func TestRobotAccountResource_NameChangeReplaces(t *testing.T) {
	api := setupFake(t)
	var oldID string
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: robotAccountHCL("ci", []string{"push"}),
				Check: func(s *terraform.State) error {
					oldID = attrs(s, robotAccountResource)["id"]
					return nil
				},
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    robotAccountHCL("ci-2", []string{"push"}),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(robotAccountResource, "name", "ci-2"),
					func(s *terraform.State) error {
						newID := attrs(s, robotAccountResource)["id"]
						if newID == "" || newID == oldID {
							return fmt.Errorf("id after the name change = %q (was %q), want a NEW id", newID, oldID)
						}
						if w := robotWritesSince(api, seen); strings.Join(w, ",") != "DELETE,POST" {
							return fmt.Errorf("writes = %v, want DELETE then POST (RequiresReplace's default order: destroy old, create new)", w)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- B4

// Fails if any plan-time validator is missing or too permissive: each case
// must be refused WITHOUT a single request reaching the robot-accounts API
// (§6 - Required+SizeAtLeast(1)+OneOf on permissions, Required+
// LengthAtLeast(1) on name).
func TestRobotAccountResource_PlanTimeValidationRefusesWithZeroRequests(t *testing.T) {
	cases := []struct {
		name, rname string
		perms       []string
		expect      string
	}{
		{"empty permissions", "ci", []string{}, "permissions"},
		{"invalid permission", "ci", []string{"admin"}, "permissions"},
		{"empty name", "", []string{"push"}, "name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{{
					Config:      robotAccountHCL(c.rname, c.perms),
					ExpectError: loose(c.expect),
				}},
			})
			for _, r := range api.Requests() {
				if strings.Contains(r.Path, "/robot-accounts") {
					t.Fatalf("%s: a robot-accounts API call was made: %s %s", c.name, r.Method, r.Path)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- B5

// Fails if Read (used by Import) does not page through the robot list to
// find an id that lands beyond the first 50, i.e. treats a short "found on
// page 1" list as exhaustive: seeds 55 filler robots (ids "00000000-...")
// that sort BEFORE the target ("ffffffff-..."), so the target can only be on
// page 2. Also fails if the provider never actually sends a `cursor` query
// parameter while paging (proving genuine pagination, not a lucky read).
func TestRobotAccountResource_ReadPagesPastFiftyToFindTheTarget(t *testing.T) {
	api := setupFake(t)
	for i := 0; i < 55; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		api.SeedRobotWithID(testOrg, id, fmt.Sprintf("filler-%d", i), []string{"push"})
	}
	targetID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	api.SeedRobotWithID(testOrg, targetID, "ci-target", []string{"push", "pull"})

	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName:       robotAccountResource,
			ImportState:        true,
			ImportStatePersist: true,
			Config:             robotAccountHCL("ci-target", []string{"push", "pull"}),
			ImportStateId:      targetID,
			ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 {
					return fmt.Errorf("want exactly one imported instance, got %d", len(is))
				}
				a := is[0].Attributes
				if a["id"] != targetID || a["name"] != "ci-target" {
					return fmt.Errorf("imported id/name = %q/%q, want %q/ci-target", a["id"], a["name"], targetID)
				}
				return nil
			},
		}},
	})

	sawCursor := false
	for _, r := range requestsMatching(api, http.MethodGet, robotsListRE) {
		if strings.Contains(r.Query, "cursor=") {
			sawCursor = true
		}
	}
	if !sawCursor {
		t.Fatalf("no GET .../robot-accounts carried a cursor query - the target (page 2 of 56 robots) cannot have been found without paging")
	}
}

// ---------------------------------------------------------------- B6

// Fails if Read does not remove a robot account deleted out of band.
func TestRobotAccountResource_ReadRemovedOutOfBandRemovesFromState(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: robotAccountHCL("ci", []string{"push"})},
			{
				PreConfig: func() {
					id := ""
					// Re-derive the id from the fake's own robot list (SetUp
					// doesn't expose one directly): find the single robot named "ci".
					_, b := rawCall(t, api, http.MethodGet, "/organizations/"+testOrg+"/registry/robot-accounts", "")
					for _, it := range rawDecode(t, b)["items"].([]any) {
						m := it.(map[string]any)
						if m["name"] == "ci" {
							id = m["id"].(string)
						}
					}
					if id == "" {
						t.Fatal("robot \"ci\" not found in the fake before removing it")
					}
					if !api.RemoveRobot(testOrg, id) {
						t.Fatalf("RemoveRobot(%s) reported the robot missing", id)
					}
				},
				Config: robotAccountHCL("ci", []string{"push"}),
				Check: resource.ComposeTestCheckFunc(
					func(*terraform.State) error {
						if posts := requestsMatching(api, http.MethodPost, robotsListRE); len(posts) != 2 {
							return fmt.Errorf("total POSTs = %d, want 2 (the original create plus the recreate)", len(posts))
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- B7

// Fails if import does not adopt every GET-derived field, if it invents a
// secret (never returned by the list), or if importing the platform's
// default "org-<slug>" robot is silently accepted instead of erroring (T11-7).
func TestRobotAccountResource_ImportAdoptsFieldsAndRefusesTheDefaultRobot(t *testing.T) {
	api := setupFake(t)
	id := api.SeedRobot(testOrg, "ci", []string{"push", "pull"})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName:       robotAccountResource,
			ImportState:        true,
			ImportStatePersist: true,
			Config:             robotAccountHCL("ci", []string{"push", "pull"}),
			ImportStateId:      id,
			ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 {
					return fmt.Errorf("want exactly one imported instance, got %d", len(is))
				}
				a := is[0].Attributes
				for k, want := range map[string]string{"id": id, "name": "ci", "harbor_username": "robot$aether-acme+ci"} {
					if a[k] != want {
						return fmt.Errorf("imported %s = %q, want %q", k, a[k], want)
					}
				}
				if v, has := a["secret"]; has {
					return fmt.Errorf("imported secret = %q, want null (the list never carries it)", v)
				}
				return nil
			},
		}},
	})
}

// Fails if importing the default "org-<slug>" robot (created by enabling the
// registry) is accepted instead of erroring with the ruled message (T11-7).
func TestRobotAccountResource_ImportOfTheDefaultRobotErrors(t *testing.T) {
	api := setupFake(t)
	rawEnableRegistry(t, api, testOrg)
	// The default robot's id is not returned by enable's response body (only
	// username/password are) - read it back from the raw robot list.
	_, b := rawCall(t, api, http.MethodGet, "/organizations/"+testOrg+"/registry/robot-accounts", "")
	var robotID string
	for _, it := range rawDecode(t, b)["items"].([]any) {
		m := it.(map[string]any)
		if m["name"] == "org-acme" {
			robotID = m["id"].(string)
		}
	}
	if robotID == "" {
		t.Fatal("default robot org-acme not found after enabling the registry")
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName:  robotAccountResource,
			ImportState:   true,
			Config:        robotAccountHCL("org-acme", []string{"repository:push", "repository:pull", "repository:delete"}),
			ImportStateId: robotID,
			ExpectError:   loose("belongs to aether_registry and cannot be managed here"),
		}},
	})
}

// ---------------------------------------------------------------- B8

// Fails if a duplicate name (a robot the fake already holds) is not surfaced
// as the API's 409 robot_exists (§6).
func TestRobotAccountResource_DuplicateNameSurfaces409(t *testing.T) {
	api := setupFake(t)
	api.SeedRobot(testOrg, "ci", []string{"push"})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      robotAccountHCL("ci", []string{"pull"}),
			ExpectError: loose("a robot account with that name already exists"),
		}},
	})
}

// ---------------------------------------------------------------- B9

// Fails if destroy sends anything but exactly one DELETE.
func TestRobotAccountResource_DestroySendsExactlyOneDelete(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: robotAccountHCL("ci", []string{"push"})},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(robotAccountResource),
					func(*terraform.State) error {
						d := filterReqs(requestsSince(api, seen), http.MethodDelete, robotOneRE)
						if len(d) != 1 {
							return fmt.Errorf("DELETEs = %v, want exactly one", d)
						}
						return nil
					},
				),
			},
		},
	})
}

// Fails if Delete treats the API's 404 (already removed out of band) as an
// error: destroy must succeed (§7 - a 404 on delete is success).
func TestRobotAccountResource_DeleteOfAnAlreadyRemovedRobotIsSuccess(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: robotAccountHCL("ci", []string{"push"})},
			{
				PreConfig: func() {
					id := attrsFromLastPlan(t, api)
					if !api.RemoveRobot(testOrg, id) {
						t.Fatalf("RemoveRobot(%s) reported the robot missing", id)
					}
				},
				Config: providerOnlyHCL,
				Check:  checkAbsentFromState(robotAccountResource),
			},
		},
	})
}

// attrsFromLastPlan re-derives the robot account's id from the fake's own
// robot list (the only "ci"-named robot at this point in the test), since
// PreConfig has no state argument to read it from directly.
func attrsFromLastPlan(t *testing.T, api *fakeapi.Server) string {
	t.Helper()
	_, b := rawCall(t, api, http.MethodGet, "/organizations/"+testOrg+"/registry/robot-accounts", "")
	for _, it := range rawDecode(t, b)["items"].([]any) {
		m := it.(map[string]any)
		if m["name"] == "ci" {
			return m["id"].(string)
		}
	}
	t.Fatal("robot \"ci\" not found in the fake")
	return ""
}

// Fails if a sticky Harbor failure on delete (retried by the transport up to
// 3 times, per TR:148-157) is swallowed instead of surfaced as an error with
// the robot account KEPT in state (§7 - Harbor-first, row survives).
func TestRobotAccountResource_DestroyStickyFailureErrorsAndKeepsState(t *testing.T) {
	api := setupFake(t)
	cfg := robotAccountHCL("ci", []string{"push"})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: cfg,
				Check: func(s *terraform.State) error {
					id := attrs(s, robotAccountResource)["id"]
					api.StickyRobotDeleteFailure(testOrg, id, http.StatusServiceUnavailable, "harbor_unavailable",
						"could not revoke the credential in the registry; not deleted")
					return nil
				},
			},
			{
				Config:      providerOnlyHCL,
				ExpectError: loose("could not revoke the credential in the registry; not deleted"),
			},
			{
				// M-1-style: the failed destroy must have kept the robot in
				// state - config on disk is still providerOnlyHCL (no resource),
				// so a post-refresh plan against a state that still has it wants
				// to destroy it again: non-empty on purpose.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					if _, ok := s.RootModule().Resources[robotAccountResource]; !ok {
						return fmt.Errorf("%s missing after the failed destroy, want it KEPT", robotAccountResource)
					}
					return nil
				},
			},
		},
	})
}
