package provider_test

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive aether_organization_member through a real terraform (or
// tofu) binary against the in-process fake (fakeapi/members.go), under the
// controller rulings T9-1..T9-4, T9-9 and T9-10 of task-9-brief-amended.md.
//
// The fake answers every member PUT with 403 token_not_permitted (contract
// §8), so a provider that ever tries an in-place role change fails loudly.
// A role or email change is therefore a REPLACE: DELETE then POST.
//
// The harness teardown destroy runs AFTER resource.UnitTest returns and
// re-applies the provider's own Delete against whatever state is left: tests
// that leave a last owner in state add a second owner first (see
// TestMemberResource_DeletingTheLastOwnerIsRefused...).
//
// Symbols the coder provides: NewOrganizationMemberResource (type name
// aether_organization_member), registered in provider.go.

const (
	memberResource = "aether_organization_member.m"
	membersListRE  = `/members$`
	memberOneRE    = `/members/[^/]+$`
)

func memberHCL(email, role string) string {
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_organization_member" "m" {
  email = %q
  role  = %q
}
`, email, role)
}

// memberWrites returns "METHOD" of every request since n that changes
// membership (POST/PUT/DELETE on a members path), in order.
func memberWrites(api *fakeapi.Server, n int) []string {
	var out []string
	for _, r := range requestsSince(api, n) {
		if r.Method != http.MethodGet && strings.Contains(r.Path, "/members") {
			out = append(out, r.Method)
		}
	}
	return out
}

func listCalls(api *fakeapi.Server, n int) int {
	c := 0
	for _, r := range requestsSince(api, n) {
		if r.Method == http.MethodGet && regexp.MustCompile(membersListRE).MatchString(r.Path) {
			c++
		}
	}
	return c
}

// ---------------------------------------------------------------- 12

// Fails if Create sends anything but {email, role} in that exact shape (an
// extra null key, an org id in the body), if it PUTs, if it lists members for
// a non-owner, or if invited_by/accepted_at are invented instead of null.
func TestMemberResource_CreateSendsExactBodyAndStateHasNullInvitedByAndAcceptedAt(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: memberHCL("alice@example.com", "viewer"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(memberResource, "email", "alice@example.com"),
				resource.TestCheckResourceAttr(memberResource, "role", "viewer"),
				resource.TestCheckResourceAttrSet(memberResource, "id"),
				resource.TestCheckResourceAttrSet(memberResource, "created_at"),
				resource.TestCheckNoResourceAttr(memberResource, "invited_by"),
				resource.TestCheckNoResourceAttr(memberResource, "accepted_at"),
				func(*terraform.State) error {
					posts := requestsMatching(api, http.MethodPost, membersListRE)
					if len(posts) != 1 {
						return fmt.Errorf("want exactly one POST, got %d", len(posts))
					}
					if got, want := compactJSON(t, posts[0].Body), `{"email":"alice@example.com","role":"viewer"}`; got != want {
						return fmt.Errorf("POST body = %s, want exactly %s", got, want)
					}
					if p := requestsMatching(api, http.MethodPut, memberOneRE); len(p) != 0 {
						return fmt.Errorf("a member PUT was sent (always 403 for tokens): %v", p)
					}
					if l := listCalls(api, 0); l != 0 {
						return fmt.Errorf("Create listed members %d time(s); only an owner delete may", l)
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- 13

// Fails if a role change is planned in place (the apply would then PUT and
// get the fake's 403), if the replacement is POST-before-DELETE (409
// duplicate email), or if the membership id is reused.
func TestMemberResource_RoleChangePlansReplaceAndAppliesDeleteThenPost(t *testing.T) {
	api := setupFake(t)
	var oldID string
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: memberHCL("alice@example.com", "viewer"),
				Check: func(s *terraform.State) error {
					oldID = s.RootModule().Resources[memberResource].Primary.Attributes["id"]
					return nil
				},
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    memberHCL("alice@example.com", "admin"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(memberResource, plancheck.ResourceActionReplace),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(memberResource, "role", "admin"),
					func(s *terraform.State) error {
						if got := strings.Join(memberWrites(api, seen), ","); got != "DELETE,POST" {
							return fmt.Errorf("apply wrote %q, want DELETE,POST in that order and no PUT", got)
						}
						if id := s.RootModule().Resources[memberResource].Primary.Attributes["id"]; id == oldID || id == "" {
							return fmt.Errorf("id = %q after replace, was %q: the membership row is new", id, oldID)
						}
						if got := api.MemberRoles(testOrg)["alice@example.com"]; got != "admin" {
							return fmt.Errorf("fake stores role %q, want admin", got)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 14

// Fails if an email change is an in-place update (no such PUT exists) or
// leaves the old address's membership behind.
func TestMemberResource_EmailChangePlansReplaceAndRemovesTheOldMembership(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: memberHCL("alice@example.com", "member")},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    memberHCL("bob@example.com", "member"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(memberResource, plancheck.ResourceActionReplace),
				}},
				Check: func(*terraform.State) error {
					if got := strings.Join(memberWrites(api, seen), ","); got != "DELETE,POST" {
						return fmt.Errorf("apply wrote %q, want DELETE,POST", got)
					}
					roles := api.MemberRoles(testOrg)
					if _, still := roles["alice@example.com"]; still || roles["bob@example.com"] != "member" {
						return fmt.Errorf("fake membership = %v, want only bob", roles)
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- 15

// Fails if the lower-case validator is missing: an upper-case address would
// otherwise be POSTed and stored verbatim, and membership matches the login
// email EXACTLY. Nothing may reach the members API.
func TestMemberResource_UpperCaseEmailIsRefusedAtPlanWithZeroCalls(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      memberHCL("Alice@Example.com", "viewer"),
			ExpectError: loose("must be lower-case: membership matches the login email exactly"),
		}},
	})
	for _, r := range api.Requests() {
		if strings.Contains(r.Path, "/members") {
			t.Fatalf("an upper-case email reached the members API: %s %s", r.Method, r.Path)
		}
	}
}

// Fails if the "exactly one @" validator is missing or too weak.
func TestMemberResource_EmailWithoutExactlyOneAtSignIsRefusedAtPlan(t *testing.T) {
	for name, email := range map[string]string{"no at": "alice.example.com", "two at": "a@b@example.com"} {
		t.Run(name, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps:                    []resource.TestStep{{Config: memberHCL(email, "viewer"), ExpectError: regexp.MustCompile(`(?is)email`)}},
			})
			if got := requestsMatching(api, http.MethodPost, membersListRE); len(got) != 0 {
				t.Fatalf("%q reached the API: %v", email, got)
			}
		})
	}
}

// Fails if the role validator is missing or admits a value outside
// owner|admin|member|viewer (including case variants).
func TestMemberResource_RoleOutsideTheFourValuesIsRefusedAtPlan(t *testing.T) {
	for _, role := range []string{"root", "Owner", ""} {
		t.Run("role="+role, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps:                    []resource.TestStep{{Config: memberHCL("a@example.com", role), ExpectError: regexp.MustCompile(`(?is)role`)}},
			})
			if got := requestsMatching(api, http.MethodPost, membersListRE); len(got) != 0 {
				t.Fatalf("role %q reached the API: %v", role, got)
			}
		})
	}
}

// Fails if owner is refused: the validator allows all four roles (T9-2).
func TestMemberResource_OwnerRoleIsAccepted(t *testing.T) {
	setupFake(t).SeedMember(testOrg, "keep@example.com", "owner") // a second owner, so teardown's guard passes
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: memberHCL("boss@example.com", "owner"),
			Check:  resource.TestCheckResourceAttr(memberResource, "role", "owner"),
		}},
	})
}

// ---------------------------------------------------------------- 16

// Fails if deleting a non-owner lists members first (the guard applies to
// owners only), skips the DELETE, or leaves the row in the fake.
func TestMemberResource_DeletingANonOwnerIsOneDeleteAndNoList(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: memberHCL("alice@example.com", "admin")},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: func(*terraform.State) error {
					if got := strings.Join(memberWrites(api, seen), ","); got != "DELETE" {
						return fmt.Errorf("destroy wrote %q, want exactly one DELETE", got)
					}
					if l := listCalls(api, seen); l != 0 {
						return fmt.Errorf("a non-owner delete listed members %d time(s)", l)
					}
					if len(api.MemberRoles(testOrg)) != 0 {
						return fmt.Errorf("member still in the fake: %v", api.MemberRoles(testOrg))
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- 17

// Fails if an owner delete skips the guard's list, or lists AFTER deleting,
// or lists more than once for a single page of members.
func TestMemberResource_DeletingAnOwnerWithAnotherOwnerListsThenDeletes(t *testing.T) {
	api := setupFake(t)
	api.SeedMember(testOrg, "second-owner@example.com", "owner")
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: memberHCL("first-owner@example.com", "owner")},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: func(*terraform.State) error {
					var order []string
					for _, r := range requestsSince(api, seen) {
						switch {
						case r.Method == http.MethodGet && regexp.MustCompile(membersListRE).MatchString(r.Path):
							order = append(order, "LIST")
						case r.Method == http.MethodDelete:
							order = append(order, "DELETE")
						}
					}
					if got := strings.Join(order, ","); got != "LIST,DELETE" {
						return fmt.Errorf("destroy did %q, want LIST,DELETE (one list, before the delete)", got)
					}
					if _, still := api.MemberRoles(testOrg)["first-owner@example.com"]; still {
						return fmt.Errorf("owner was not removed")
					}
					return nil
				},
			},
		},
	})
}

// Fails if the guard reads only the first page of the member list: the one
// OTHER owner sorts after 150 non-owners, so on any page size <= 100 it is
// on a later page, and a first-page-only guard would wrongly refuse.
func TestMemberResource_LastOwnerGuardPaginatesToFindAnotherOwner(t *testing.T) {
	api := setupFake(t)
	for i := 0; i < 150; i++ {
		api.SeedMember(testOrg, fmt.Sprintf("viewer%03d@example.com", i), "viewer")
	}
	api.SeedMemberWithID(testOrg, "ffffffff-ffff-4fff-8fff-ffffffffffff", "far-owner@example.com", "owner")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: memberHCL("first-owner@example.com", "owner")},
			{
				Config: providerOnlyHCL,
				Check: func(*terraform.State) error {
					if _, still := api.MemberRoles(testOrg)["first-owner@example.com"]; still {
						return fmt.Errorf("owner not removed although another owner exists on a later page")
					}
					if l := listCalls(api, 0); l < 2 {
						return fmt.Errorf("guard made %d list call(s); with 152 members and a page size <= 100 it must follow next_cursor", l)
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- 18

// Fails if the last owner can be removed (the API has no guard), if the
// refusal drifts from the ruled text, or if any DELETE is sent before the
// refusal. Recovery afterwards proves the state was left intact.
func TestMemberResource_DeletingTheLastOwnerIsRefusedAndSendsNoDelete(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: memberHCL("only-owner@example.com", "owner")},
			{
				Config: providerOnlyHCL,
				ExpectError: loose("refusing to remove the last owner: the API has no guard (aether-api #302) " +
					"and the organization would become unmanageable (roles and deletion need an owner login)"),
			},
			{
				PreConfig: func() {
					if d := requestsMatching(api, http.MethodDelete, `.*`); len(d) != 0 {
						t.Errorf("a DELETE was sent although the last owner was refused: %v", d)
					}
					if len(api.MemberRoles(testOrg)) != 1 {
						t.Errorf("fake members = %v, want the owner still present", api.MemberRoles(testOrg))
					}
					// let the harness' teardown destroy pass the guard
					api.SeedMember(testOrg, "second-owner@example.com", "owner")
				},
				Config: memberHCL("only-owner@example.com", "owner"), PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// Fails if a REPLACE of the last owner (role change owner -> admin) bypasses
// the guard: replace = delete first, so the org would lose its only owner.
func TestMemberResource_ReplacingTheLastOwnerIsRefusedToo(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: memberHCL("only-owner@example.com", "owner")},
			{
				Config:      memberHCL("only-owner@example.com", "admin"),
				ExpectError: loose("refusing to remove the last owner"),
			},
			{
				PreConfig: func() {
					if d := requestsMatching(api, http.MethodDelete, `.*`); len(d) != 0 {
						t.Errorf("a DELETE was sent although the last owner was refused: %v", d)
					}
					api.SeedMember(testOrg, "second-owner@example.com", "owner")
				},
				Config: memberHCL("only-owner@example.com", "owner"), PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// ---------------------------------------------------------------- 19

// Fails if the API's 409 is swallowed, mapped to a different message, or
// leaves state behind (a failed Create must save nothing).
func TestMemberResource_DuplicateEmailSurfacesThe409AndLeavesNoState(t *testing.T) {
	api := setupFake(t)
	api.SeedMember(testOrg, "dup@example.com", "viewer")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: memberHCL("dup@example.com", "admin"), ExpectError: loose("Member already exists")},
			{Config: providerOnlyHCL, PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
	if d := requestsMatching(api, http.MethodDelete, `.*`); len(d) != 0 {
		t.Fatalf("a failed Create left something to delete (or deleted the seeded member): %v", d)
	}
	if got := api.MemberRoles(testOrg)["dup@example.com"]; got != "viewer" {
		t.Fatalf("the pre-existing member's role became %q", got)
	}
}

// ---------------------------------------------------------------- 20

// Fails if import does not read the membership by id (email/role/created_at
// unset), invents invited_by/accepted_at, writes, or sets a different id.
func TestMemberResource_ImportByMembershipIDReadsTheMember(t *testing.T) {
	api := setupFake(t)
	id := api.SeedMember(testOrg, "imported@example.com", "admin")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName: memberResource, ImportState: true, ImportStatePersist: true,
			Config: memberHCL("imported@example.com", "admin"), ImportStateId: id,
			ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 {
					return fmt.Errorf("want one imported instance, got %d", len(is))
				}
				a := is[0].Attributes
				for k, want := range map[string]string{"id": id, "email": "imported@example.com", "role": "admin"} {
					if a[k] != want {
						return fmt.Errorf("imported %s = %q, want %q", k, a[k], want)
					}
				}
				if a["created_at"] == "" {
					return fmt.Errorf("imported created_at is empty")
				}
				for _, k := range []string{"invited_by", "accepted_at"} {
					if v, has := a[k]; has {
						return fmt.Errorf("imported %s = %q, want null", k, v)
					}
				}
				if w := writes(api); len(w) != 0 {
					return fmt.Errorf("import wrote: %v", w)
				}
				return nil
			},
		}},
	})
}

// ---------------------------------------------------------------- 21

// Fails if a 404 on Read (membership removed out of band) leaves the resource
// in state, or errors instead: the next plan must recreate it, and the
// recreate must really POST.
func TestMemberResource_Read404RemovesFromStateAndTheNextPlanRecreates(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: memberHCL("alice@example.com", "viewer"),
				Check: func(s *terraform.State) error {
					id = s.RootModule().Resources[memberResource].Primary.Attributes["id"]
					return nil
				},
			},
			{
				PreConfig: func() { api.RemoveMember(testOrg, id) },
				Config:    memberHCL("alice@example.com", "viewer"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(memberResource, plancheck.ResourceActionCreate),
				}},
				Check: func(*terraform.State) error {
					if posts := requestsMatching(api, http.MethodPost, membersListRE); len(posts) != 2 {
						return fmt.Errorf("want the original POST plus the recreate's, got %d", len(posts))
					}
					return nil
				},
			},
		},
	})
}

// Fails if Delete treats the API's 404 (already removed out of band) as an
// error: the destroy must succeed.
func TestMemberResource_DeleteOfAnAlreadyRemovedMemberIsSuccess(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: memberHCL("alice@example.com", "viewer"),
				Check: func(s *terraform.State) error {
					id = s.RootModule().Resources[memberResource].Primary.Attributes["id"]
					return nil
				},
			},
			{
				// The refresh would notice the removal and skip Delete entirely,
				// so the read is forced to succeed while the DELETE itself sees a
				// 404: exactly a removal between the refresh and the delete.
				PreConfig: func() {
					api.Handlers["DELETE /organizations/"+testOrg+"/members/"+id] = func(w http.ResponseWriter, r *http.Request) {
						api.RemoveMember(testOrg, id)
						fakeapi.WriteJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "Member not found"}})
					}
				},
				Config: providerOnlyHCL,
				Check:  resource.TestCheckNoResourceAttr(memberResource, "id"),
			},
		},
	})
}

// ---------------------------------------------------------------- read hygiene

// Fails if Read (or the whole apply) misreports a 403 (wrong org for this
// token) as gone: it must be an error, with state kept.
func TestMemberResource_Read403IsAnErrorAndKeepsState(t *testing.T) {
	api := setupFake(t)
	var route string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: memberHCL("alice@example.com", "viewer"),
				Check: func(s *terraform.State) error {
					route = "GET /organizations/" + testOrg + "/members/" + s.RootModule().Resources[memberResource].Primary.Attributes["id"]
					return nil
				},
			},
			{
				PreConfig: func() {
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "forbidden", "message": "no access to this organization for this token"}})
					}
				},
				Config:      memberHCL("alice@example.com", "viewer"),
				ExpectError: loose("no access to this organization for this token"),
			},
			{
				PreConfig: func() { delete(api.Handlers, route) },
				Config:    memberHCL("alice@example.com", "viewer"), PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}
