package provider_test

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
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

// summaryThenDetail matches a diagnostic whose summary is followed by each
// detail phrase in order, anything in between (terraform's "with ..." source
// context, a request id), each phrase matched as loose() does.
func summaryThenDetail(summary string, detail ...string) *regexp.Regexp {
	re := loose(summary).String()
	for _, d := range detail {
		re += `.*` + strings.TrimPrefix(loose(d).String(), `(?is)`)
	}
	return regexp.MustCompile(re)
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
// otherwise be POSTed and stored verbatim, and the provider manages member
// addresses only in their canonical lower-case form (so configuration and the
// stored address never differ only in case). Nothing may reach the members API.
func TestMemberResource_UpperCaseEmailIsRefusedAtPlanWithZeroCalls(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      memberHCL("Alice@Example.com", "viewer"),
			ExpectError: loose("must be lower-case: the provider manages member addresses in their canonical lower-case form"),
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

// Fails if the last owner can be removed (the provider refuses before sending
// anything, whether or not the API also refuses), if the refusal's summary or
// detail drifts from the ruled text, or if any DELETE is sent before the
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
				ExpectError: summaryThenDetail("Cannot remove the organization's last owner",
					"refusing to remove the last owner: the organization would become unmanageable "+
						"(roles and deletion need an owner login)"),
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
				Check:  checkAbsentFromState(memberResource),
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

// ---------------------------------------------------------------- review r1 (M-3, I-1, M-4)

// destroyStepRefreshGets is how many GETs of a member the harness itself makes
// during ONE destroy/apply step BEFORE the provider's Delete runs: only the
// plan process' refresh - the saved plan is applied WITHOUT another refresh.
// Re-measured (review r1b) on terraform 1.5.7 AND tofu 1.12.6 with
// plugin-testing v1.16.0, request log after the PreConfig hook: GET org list,
// GET /members/{id} (refresh), GET org list (apply's Configure), GET
// /members/{id} (Delete's own re-read), GET /members (guard list), DELETE. (An
// earlier reading of two refresh GETs mistook Delete's re-read for a refresh.) A GET beyond this count therefore comes from
// Delete itself - the guard's own live re-read - which is what the scripted
// handlers below exploit to tell the two apart. If a plugin-testing upgrade
// changes the count these tests fail loudly (the refresh would see the
// guard-only answer), they do not pass vacuously.
const destroyStepRefreshGets = 1

// scriptedMemberGet answers successive GETs of one member from roles: a role
// name serves that role, "404" serves a 404. The last entry repeats. It
// replaces the fake's own answer, so the script can differ from what the
// fake (and the seeded list) really holds.
func scriptedMemberGet(api *fakeapi.Server, id, email string, roles ...string) {
	var mu sync.Mutex
	n := 0
	api.Handlers["GET /organizations/"+testOrg+"/members/"+id] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := min(n, len(roles)-1)
		n++
		mu.Unlock()
		if roles[i] == "404" {
			fakeapi.WriteJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "Member not found"}})
			return
		}
		fakeapi.WriteJSON(w, http.StatusOK, map[string]any{"id": id, "org_id": testOrg, "email": email, "role": roles[i], "created_at": "2026-09-23T00:00:01Z"})
	}
}

const (
	memberListRoute = "GET /organizations/" + testOrg + "/members"
	otherMemberID   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

func listPage(cursor string) map[string]any {
	page := map[string]any{"items": []map[string]any{{"id": otherMemberID, "org_id": testOrg, "email": "viewer@example.com", "role": "viewer", "created_at": "2026-09-23T00:00:02Z"}}}
	if cursor != "" {
		page["next_cursor"] = cursor
	}
	return page
}

// createSoleOwner is step 1 of the guard-path tests: one owner, the only one.
func createSoleOwner(api *fakeapi.Server, idOut *string) resource.TestStep {
	return resource.TestStep{
		Config: memberHCL("solo@example.com", "owner"),
		Check: func(s *terraform.State) error {
			*idOut = s.RootModule().Resources[memberResource].Primary.Attributes["id"]
			return nil
		},
	}
}

// recoverStep clears the overrides, adds a second owner so the harness'
// teardown destroy passes the guard, and asserts the refused destroy sent no DELETE
// and left the member in the fake.
func recoverStep(t *testing.T, api *fakeapi.Server, id string) resource.TestStep {
	return resource.TestStep{
		PreConfig: func() {
			delete(api.Handlers, memberListRoute)
			delete(api.Handlers, "GET /organizations/"+testOrg+"/members/"+id)
			if d := requestsMatching(api, http.MethodDelete, `.*`); len(d) != 0 {
				t.Errorf("a DELETE was sent although the guard refused/failed: %v", d)
			}
			api.SeedMember(testOrg, "second-owner@example.com", "owner")
		},
		Config: memberHCL("solo@example.com", "owner"), PlanOnly: true, ExpectNonEmptyPlan: false,
	}
}

// M-3(a). Fails if a 404 on the guard's own live re-read is turned into a
// refusal or into a DELETE: the member is already gone, so destroy must
// succeed and send no DELETE. The refresh sees the owner (so Delete runs);
// only the guard's re-read sees the 404.
func TestMemberResource_GuardTreatsALiveReReadOf404AsAlreadyGone(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			createSoleOwner(api, &id),
			{
				PreConfig: func() {
					roles := make([]string, 0, destroyStepRefreshGets+1)
					for i := 0; i < destroyStepRefreshGets; i++ {
						roles = append(roles, "owner")
					}
					scriptedMemberGet(api, id, "solo@example.com", append(roles, "404")...)
				},
				Config: providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(memberResource),
					func(*terraform.State) error {
						if d := requestsMatching(api, http.MethodDelete, `.*`); len(d) != 0 {
							return fmt.Errorf("DELETE sent for an already-gone member: %v", d)
						}
						return nil
					},
				),
			},
		},
	})
}

// M-3(b). Fails if a member that is missing from the COMPLETE list (yet was
// readable a moment ago) is refused or DELETEd: it is already gone.
func TestMemberResource_GuardTreatsAMemberMissingFromTheListAsAlreadyGone(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			createSoleOwner(api, &id),
			{
				PreConfig: func() {
					api.Handlers[memberListRoute] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{}})
					}
				},
				Config: providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(memberResource),
					func(*terraform.State) error {
						if d := requestsMatching(api, http.MethodDelete, `.*`); len(d) != 0 {
							return fmt.Errorf("DELETE sent for a member absent from the list: %v", d)
						}
						return nil
					},
				),
			},
		},
	})
}

// M-3(c). Fails if a failing member list is swallowed (DELETE anyway, or
// reported as success): the guard cannot prove another owner exists, so it must
// error and send no DELETE.
func TestMemberResource_GuardListFailureIsAnErrorWithNoDelete(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			createSoleOwner(api, &id),
			{
				PreConfig: func() {
					api.Handlers[memberListRoute] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal_error", "message": "list exploded"}})
					}
				},
				Config:      providerOnlyHCL,
				ExpectError: loose("list exploded"),
			},
			recoverStep(t, api, id),
		},
	})
}

// M-3(d1). Fails if a next_cursor that is not a UUID is followed blindly or
// ignored (a DELETE would follow): the guard must error, after one list call.
func TestMemberResource_GuardUnparseableCursorIsAnErrorWithNoDelete(t *testing.T) {
	api := setupFake(t)
	var id string
	var mu sync.Mutex
	calls := 0
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			createSoleOwner(api, &id),
			{
				PreConfig: func() {
					api.Handlers[memberListRoute] = func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						calls++
						mu.Unlock()
						fakeapi.WriteJSON(w, http.StatusOK, listPage("not-a-uuid"))
					}
				},
				Config:      providerOnlyHCL,
				ExpectError: regexp.MustCompile(`(?is)cursor`),
			},
			recoverStep(t, api, id),
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("list called %d times, want 1 (an unusable cursor ends the guard)", calls)
	}
}

// M-3(d2). Fails if a cursor that never advances (the same next_cursor forever)
// makes the guard loop: it must stop (error) within a few calls. The fake
// answers 500 from the 4th call on, so a runaway loop ends the test with a
// failure instead of hanging it.
func TestMemberResource_GuardNonAdvancingCursorDoesNotLoop(t *testing.T) {
	api := setupFake(t)
	var id string
	var mu sync.Mutex
	calls := 0
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			createSoleOwner(api, &id),
			{
				PreConfig: func() {
					api.Handlers[memberListRoute] = func(w http.ResponseWriter, r *http.Request) {
						mu.Lock()
						calls++
						n := calls
						mu.Unlock()
						if n > 3 {
							fakeapi.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal_error", "message": "runaway list"}})
							return
						}
						fakeapi.WriteJSON(w, http.StatusOK, listPage("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
					}
				},
				Config:      providerOnlyHCL,
				ExpectError: regexp.MustCompile(`(?s).`),
			},
			recoverStep(t, api, id),
		},
	})
	mu.Lock()
	defer mu.Unlock()
	if calls > 3 {
		t.Fatalf("list called %d times for a cursor that never advances, want <= 3 (a bounded guard)", calls)
	}
}

// I-1. Fails if the guard trusts the ROLE IN STATE instead of re-reading the
// member: the refresh (the harness' one GET) is scripted to report `member`
// while the fake - and every later GET - says `owner` (the only owner). Only
// a Delete that re-reads the live member sees the owner and refuses. Mechanism:
// GETs 1..destroyStepRefreshGets come from the harness' refresh and are the
// only ones that can reach state; the (n+1)th GET can only be Delete's own.
// The refusal must come with zero DELETEs.
func TestMemberResource_GuardReadsTheLiveRoleNotTheRoleInState(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: memberHCL("solo@example.com", "member"),
				Check: func(s *terraform.State) error {
					id = s.RootModule().Resources[memberResource].Primary.Attributes["id"]
					return nil
				},
			},
			{
				PreConfig: func() {
					if !api.SetMemberRole(testOrg, id, "owner") { // out of band: now the only owner
						t.Errorf("member %s missing from the fake", id)
					}
					roles := make([]string, 0, destroyStepRefreshGets+1)
					for i := 0; i < destroyStepRefreshGets; i++ {
						roles = append(roles, "member") // what the refresh writes into state
					}
					scriptedMemberGet(api, id, "solo@example.com", append(roles, "owner")...)
				},
				Config:      providerOnlyHCL,
				ExpectError: loose("refusing to remove the last owner"),
			},
			{
				// An APPLY step, not PlanOnly: step 2's refresh wrote the
				// scripted role=member into state, so a PlanOnly check (which also
				// plans with -refresh=false) would always see a replace against
				// role=owner. The apply refreshes first (the real fake says owner),
				// finds no diff, and leaves state matching for the teardown.
				PreConfig: func() {
					delete(api.Handlers, "GET /organizations/"+testOrg+"/members/"+id)
					if d := requestsMatching(api, http.MethodDelete, `.*`); len(d) != 0 {
						t.Errorf("a DELETE was sent for the live last owner: %v", d)
					}
					api.SeedMember(testOrg, "second-owner@example.com", "owner")
				},
				Config: memberHCL("solo@example.com", "owner"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
			},
		},
	})
}

// M-4. Fails if a malformed or non-canonical membership id is accepted by
// import (the API would then be asked about it) or refused with another text.
func TestMemberResource_ImportWithAMalformedIDIsRefusedWithZeroMemberCalls(t *testing.T) {
	for name, mk := range map[string]func(realID string) string{
		"not a uuid":   func(string) string { return "not-a-uuid" },
		"upper case":   strings.ToUpper,
		"braced":       func(id string) string { return "{" + id + "}" },
		"missing dash": func(id string) string { return strings.ReplaceAll(id, "-", "") },
	} {
		t.Run(name, func(t *testing.T) {
			api := setupFake(t)
			bad := mk(api.SeedMember(testOrg, "imp@example.com", "viewer"))
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{{
					ResourceName: memberResource, ImportState: true,
					Config: memberHCL("imp@example.com", "viewer"), ImportStateId: bad,
					ExpectError: loose("Invalid import id"),
				}},
			})
			for _, r := range api.Requests() {
				if strings.Contains(r.Path, "/members") {
					t.Fatalf("import id %q reached the members API: %s %s", bad, r.Method, r.Path)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- organization role enforcement

// Fails if the API's 409 last_owner on the DELETE is swallowed, reported under
// another summary, or drops the membership from state. Scenario: the
// provider's guard lists the members and sees a second owner, and that owner
// is removed (in the portal, or by another run) before the DELETE arrives -
// the case the provider guard cannot reach and the API refuses. The handler
// below serves the guard's list with both owners and removes the second one
// from the fake as it answers. Exactly one DELETE proves the refusal came
// from the API, not from the provider guard (which sends none).
func TestMemberResource_ConcurrentLastOwnerRemovalSurfacesTheAPIRefusalAndKeepsState(t *testing.T) {
	api := setupFake(t)
	api.SetLastOwnerEnforced(testOrg)
	const secondID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	api.SeedMemberWithID(testOrg, secondID, "second-owner@example.com", "owner")
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: memberHCL("first-owner@example.com", "owner"),
				Check: func(s *terraform.State) error {
					id = s.RootModule().Resources[memberResource].Primary.Attributes["id"]
					return nil
				},
			},
			{
				PreConfig: func() {
					api.Handlers[memberListRoute] = func(w http.ResponseWriter, r *http.Request) {
						api.RemoveMember(testOrg, secondID) // removed elsewhere, after this list
						fakeapi.WriteJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{
							{"id": id, "org_id": testOrg, "email": "first-owner@example.com", "role": "owner", "created_at": "2026-09-23T00:00:01Z"},
							{"id": secondID, "org_id": testOrg, "email": "second-owner@example.com", "role": "owner", "created_at": "2026-09-23T00:00:02Z"},
						}})
					}
				},
				Config: providerOnlyHCL,
				ExpectError: summaryThenDetail("Cannot remove the organization's last owner",
					"The organization's last owner cannot be removed or demoted — add another owner first",
					"Add another owner first, then remove or replace this one in a later apply."),
			},
			{
				PreConfig: func() {
					delete(api.Handlers, memberListRoute)
					if d := requestsMatching(api, http.MethodDelete, memberOneRE); len(d) != 1 {
						t.Errorf("want exactly one DELETE (the one the API refused), got %d: %v", len(d), d)
					}
					if got := api.MemberRoles(testOrg)["first-owner@example.com"]; got != "owner" {
						t.Errorf("fake role = %q, want the refused owner still present", got)
					}
					api.SeedMember(testOrg, "third-owner@example.com", "owner") // lets the teardown destroy pass
				},
				// State kept: the same configuration plans nothing.
				Config: memberHCL("first-owner@example.com", "owner"), PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// Fails if the provider treats an address that differs from an existing
// member's only in letter case as new, swallows the API's 409, or saves state
// for the refused create. The existing member was stored with upper case out
// of band (the API stores the address as written); the configuration uses the
// lower-case form the provider requires.
func TestMemberResource_CaseVariantOfAnExistingMemberSurfacesThe409AndSavesNothing(t *testing.T) {
	api := setupFake(t)
	api.SeedMember(testOrg, "Alice@Example.com", "viewer")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: memberHCL("alice@example.com", "admin"), ExpectError: summaryThenDetail("Conflict", "Member already exists")},
			{Config: providerOnlyHCL, PlanOnly: true, ExpectNonEmptyPlan: false},
		},
	})
	if posts := requestsMatching(api, http.MethodPost, membersListRE); len(posts) != 1 {
		t.Fatalf("want exactly one POST, got %d", len(posts))
	}
	if d := requestsMatching(api, http.MethodDelete, `.*`); len(d) != 0 {
		t.Fatalf("a refused create left something to delete: %v", d)
	}
	if got := api.MemberRoles(testOrg); len(got) != 1 || got["Alice@Example.com"] != "viewer" {
		t.Fatalf("fake members = %v, want only the pre-existing Alice@Example.com viewer", got)
	}
}

// Fails if a 403 insufficient_role on a write is reported with the generic
// "Aether API rejected the request ... Check api_token" diagnostic (the token
// was accepted; its organization role is what is short), or without the
// API's message. ExpectError cannot assert an absence, so the step's error is
// inspected by the case's ErrorCheck instead.
func TestMemberResource_InsufficientRoleOnAWriteSurfacesTheRoleDiagnosticNotTheTokenHint(t *testing.T) {
	api := setupFake(t)
	api.RefuseWritesForInsufficientRole()
	var got error
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		ErrorCheck: func(err error) error {
			got = err
			return nil
		},
		Steps: []resource.TestStep{{Config: memberHCL("alice@example.com", "viewer")}},
	})
	if got == nil {
		t.Fatal("the apply succeeded although every write is refused")
	}
	msg := got.Error()
	if re := summaryThenDetail("The API token's organization role does not allow this operation",
		"Your role in this organization does not permit this action",
		"Use an API token with the Admin role (Settings → API Tokens); changing a member's role is open to no token and must be done in the portal, signed in as an organization owner."); !re.MatchString(msg) {
		t.Fatalf("error lacks the role diagnostic (%s):\n%s", re, msg)
	}
	for _, generic := range []string{"api_token", "Aether API rejected the request"} {
		if strings.Contains(msg, generic) {
			t.Fatalf("error carries the generic token diagnostic %q:\n%s", generic, msg)
		}
	}
	if posts := requestsMatching(api, http.MethodPost, membersListRE); len(posts) != 1 {
		t.Fatalf("want the one refused POST, got %d", len(posts))
	}
	if len(api.MemberRoles(testOrg)) != 0 {
		t.Fatalf("fake members = %v, want none", api.MemberRoles(testOrg))
	}
}
