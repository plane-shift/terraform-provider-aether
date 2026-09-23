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

// These tests drive aether_organization through a real terraform (or tofu)
// binary against the in-process fake (fakeapi/organizations.go), which
// reproduces what aether-api DOES (SDD task-9-measured-contract.md), under the
// controller rulings T9-5..T9-8 of task-9-brief-amended.md.
//
// The organization cannot be created (T9-8), so EVERY test that needs state
// gets it from an ImportState step with ImportStatePersist. The harness'
// end-of-test teardown destroy runs AFTER resource.UnitTest returns; Delete is
// state-only, so it must add zero requests.
//
// Symbols the coder provides: NewOrganizationResource (type name
// aether_organization), registered in provider.go.

const (
	orgResource = "aether_organization.o"
	orgURLRE    = `^/organizations/[^/]+$`
)

// orgHCL renders the organization under test; extra lines go inside the block.
func orgHCL(extra ...string) string {
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_organization" "o" {
%s
}
`, strings.Join(extra, "\n"))
}

const providerOnlyHCL = `provider "aether" {}`

// importOrgStep adopts the provider's organization into state.
func importOrgStep(check resource.ImportStateCheckFunc) resource.TestStep {
	return resource.TestStep{
		ResourceName: orgResource, ImportState: true, ImportStatePersist: true,
		Config: orgHCL(), ImportStateId: testOrg, ImportStateCheck: check,
	}
}

// writesTo returns the recorded non-GET requests other than provider
// configuration: anything that could change the API.
func writes(api *fakeapi.Server) []fakeapi.Request {
	var out []fakeapi.Request
	for _, r := range api.Requests() {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// requestsSince returns requests recorded after the first n.
func requestsSince(api *fakeapi.Server, n int) []fakeapi.Request { return api.Requests()[n:] }

func strp(s string) *string { return &s }

// ---------------------------------------------------------------- 1

// Fails if Create is implemented (any request other than Configure's
// organization list is made), or if its diagnostic drifts from the plan's
// exact wording, or lacks the import hint.
func TestOrganizationResource_CreateIsRefusedWithTheExactMessageAndZeroWrites(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      orgHCL(`name = "Acme"`),
			ExpectError: loose("Organizations are created at registration"),
		}},
	})
	if w := writes(api); len(w) != 0 {
		t.Fatalf("a refused Create sent writes: %v", w)
	}
	if got := requestsMatching(api, http.MethodGet, orgURLRE); len(got) != 0 {
		t.Fatalf("a refused Create read the organization: %v", got)
	}
}

// Fails if the refusal does not tell the practitioner how to proceed.
func TestOrganizationResource_CreateRefusalNamesTheImportCommand(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      orgHCL(),
			ExpectError: loose("terraform import aether_organization"),
		}},
	})
}

// ---------------------------------------------------------------- 2

// Fails if import maps an absent billing_email to "" (or anything but null),
// loses a present-but-empty vat_id, drops plan/created_at/updated_at, sets the
// wrong id, or writes anything.
func TestOrganizationResource_ImportReadsTheOrganizationAndMapsAbsentToNull(t *testing.T) {
	api := setupFake(t)
	api.SetOrgBilling(testOrg, nil, strp("")) // billing_email NULL (absent), vat_id ""
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{importOrgStep(func(is []*terraform.InstanceState) error {
			if len(is) != 1 {
				return fmt.Errorf("want one imported instance, got %d", len(is))
			}
			a := is[0].Attributes
			for k, want := range map[string]string{"id": testOrg, "name": "Acme", "slug": "acme", "plan": "starter", "vat_id": ""} {
				if got, has := a[k]; !has || got != want {
					return fmt.Errorf("imported %s = %q (present=%v), want %q", k, got, has, want)
				}
			}
			if v, has := a["billing_email"]; has {
				return fmt.Errorf("imported billing_email = %q, want null (the API omits it while NULL)", v)
			}
			for _, k := range []string{"created_at", "updated_at"} {
				if a[k] == "" {
					return fmt.Errorf("imported %s is empty", k)
				}
			}
			if w := writes(api); len(w) != 0 {
				return fmt.Errorf("import wrote: %v", w)
			}
			return nil
		})},
	})
}

// ---------------------------------------------------------------- 3

// Fails if Update sends anything but the one changed attribute (slug echoed
// unchanged, billing_email/vat_id as null or "", name plus extras), if it does
// not PUT at all, if updated_at is not refreshed (an "inconsistent result
// after apply" error, or a stale value), or if the follow-up plan is not empty.
func TestOrganizationResource_UpdatingNameSendsExactlyThatKeyAndRefreshesUpdatedAt(t *testing.T) {
	api := setupFake(t)
	var before string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			importOrgStep(func(is []*terraform.InstanceState) error {
				before = is[0].Attributes["updated_at"]
				return nil
			}),
			{
				Config: orgHCL(`name = "Acme Renamed"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(orgResource, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(orgResource, "name", "Acme Renamed"),
					resource.TestCheckResourceAttr(orgResource, "slug", "acme"),
					resource.TestCheckResourceAttr(orgResource, "plan", "starter"),
					func(s *terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, orgURLRE)
						if len(puts) != 1 {
							return fmt.Errorf("want exactly one PUT, got %d", len(puts))
						}
						if got, want := compactJSON(t, puts[0].Body), `{"name":"Acme Renamed"}`; got != want {
							return fmt.Errorf("PUT body = %s, want exactly %s", got, want)
						}
						after := s.RootModule().Resources[orgResource].Primary.Attributes["updated_at"]
						if after == "" || after == before {
							return fmt.Errorf("updated_at = %q after the PUT, was %q: it must be re-read from the response", after, before)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 4

// Fails if "" in config is sent as null/omitted (the API would KEEP the old
// value), if state then holds null instead of "", or if the harness' own
// post-apply plan is non-empty (the "" would read back and drift).
func TestOrganizationResource_EmptyStringClearsBillingEmailAndIsStoredAsEmpty(t *testing.T) {
	api := setupFake(t)
	api.SetOrgBilling(testOrg, strp("old@example.com"), nil)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			importOrgStep(func(is []*terraform.InstanceState) error {
				if is[0].Attributes["billing_email"] != "old@example.com" {
					return fmt.Errorf("seed not imported: %v", is[0].Attributes)
				}
				return nil
			}),
			{
				Config: orgHCL(`billing_email = ""`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(orgResource, "billing_email", ""),
					func(*terraform.State) error {
						puts := requestsMatching(api, http.MethodPut, orgURLRE)
						if len(puts) != 1 {
							return fmt.Errorf("want exactly one PUT, got %d", len(puts))
						}
						if got, want := compactJSON(t, puts[0].Body), `{"billing_email":""}`; got != want {
							return fmt.Errorf("PUT body = %s, want exactly %s", got, want)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 5

// Fails if removing billing_email from config is treated as "clear" (a PUT of
// "" or null) or plans a change: the attribute is Optional+Computed, so
// dropping it means "stop managing" and keeps the stored value.
func TestOrganizationResource_RemovingBillingEmailFromConfigSendsNothingAndKeepsTheValue(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			importOrgStep(nil),
			{
				Config: orgHCL(`billing_email = "kept@example.com"`),
				Check:  resource.TestCheckResourceAttr(orgResource, "billing_email", "kept@example.com"),
			},
			{
				Config: orgHCL(),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(orgResource, "billing_email", "kept@example.com"),
					func(*terraform.State) error {
						if puts := requestsMatching(api, http.MethodPut, orgURLRE); len(puts) != 1 {
							return fmt.Errorf("want only the one PUT that set the value, got %d: removing the attribute must send nothing", len(puts))
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 6

// Fails if a slug change reaches the API (PUT) or is planned as an in-place
// update: a rename strands registry images, storage billing and quota for
// existing clusters (aether-api #303), so it is refused at PLAN time.
func TestOrganizationResource_ChangingTheSlugIsRefusedAtPlanWithNoWrites(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			importOrgStep(nil),
			{
				PreConfig:   func() { seen = len(api.Requests()) },
				Config:      orgHCL(`slug = "acme-renamed"`),
				ExpectError: loose("aether-api #303"),
			},
		},
	})
	for _, r := range requestsSince(api, seen) {
		if r.Method != http.MethodGet {
			t.Fatalf("a refused slug change sent %s %s: %s", r.Method, r.Path, r.Body)
		}
	}
	if puts := requestsMatching(api, http.MethodPut, orgURLRE); len(puts) != 0 {
		t.Fatalf("PUTs = %v, want none", puts)
	}
}

// Fails if setting slug to its CURRENT value is refused: only a differing
// slug is a rename (ModifyPlan must compare, not just detect presence).
func TestOrganizationResource_RestatingTheCurrentSlugIsAcceptedAndSendsNothing(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			importOrgStep(nil),
			{
				Config: orgHCL(`slug = "acme"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
				Check: func(*terraform.State) error {
					if w := writes(api); len(w) != 0 {
						return fmt.Errorf("an unchanged slug sent writes: %v", w)
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- 7

// Fails if the provider stops validating slug format itself (the API accepts
// anything): each case must be refused at plan with the attribute named, and
// with nothing but Configure's list request sent (were the validator missing,
// the run would instead reach Create's refusal, which does not mention slug).
func TestOrganizationResource_SlugFormatIsRefusedAtPlan(t *testing.T) {
	for name, slug := range map[string]string{
		"upper-case":    "Acme",
		"space":         "acme corp",
		"leading dash":  "-acme",
		"trailing dash": "acme-",
		"single char":   "a",
		"empty":         "",
		"underscore":    "ac_me",
		"64 characters": strings.Repeat("a", 64),
	} {
		t.Run(name, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{{
					Config:      orgHCL(fmt.Sprintf(`slug = %q`, slug)),
					ExpectError: regexp.MustCompile(`(?is)slug`),
				}},
			})
			if got := requestsMatching(api, http.MethodGet, orgURLRE); len(got) != 0 {
				t.Fatalf("slug %q reached the API: %v", slug, got)
			}
			if w := writes(api); len(w) != 0 {
				t.Fatalf("slug %q sent writes: %v", slug, w)
			}
		})
	}
}

// Fails if the other config validators are missing: name must be non-empty
// when set, and billing_email must contain exactly one @ when non-empty.
func TestOrganizationResource_NameAndBillingEmailValidatorsRefuseAtPlan(t *testing.T) {
	for name, c := range map[string]struct {
		attr string
		want *regexp.Regexp
	}{
		"empty name":       {`name = ""`, regexp.MustCompile(`(?is)name`)},
		"email without @":  {`billing_email = "nobody"`, regexp.MustCompile(`(?is)billing_email`)},
		"email with two @": {`billing_email = "a@b@c"`, regexp.MustCompile(`(?is)billing_email`)},
	} {
		t.Run(name, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps:                    []resource.TestStep{{Config: orgHCL(c.attr), ExpectError: c.want}},
			})
			if w := writes(api); len(w) != 0 || len(requestsMatching(api, http.MethodGet, orgURLRE)) != 0 {
				t.Fatalf("%s reached the API: %v", c.attr, api.Requests())
			}
		})
	}
}

// ---------------------------------------------------------------- 8

// Fails if destroy calls the API (DELETE would be a 403 for every token; PUT
// or GET-then-write would be surprising), or if it does not remove the
// resource from state. The organization itself must never be deleted.
func TestOrganizationResource_DestroyIsStateOnlyAndMakesNoWrites(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			importOrgStep(nil),
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(orgResource),
					func(*terraform.State) error {
						for _, r := range requestsSince(api, seen) {
							if r.Method != http.MethodGet {
								return fmt.Errorf("destroy sent %s %s", r.Method, r.Path)
							}
						}
						return nil
					},
				),
			},
		},
	})
	if w := writes(api); len(w) != 0 {
		t.Fatalf("writes = %v, want none over the whole test (incl. teardown)", w)
	}
	if got := requestsMatching(api, http.MethodDelete, `.*`); len(got) != 0 {
		t.Fatalf("DELETE sent: %v", got)
	}
}

// ---------------------------------------------------------------- 9

// Fails if a 403 on Read (the wrong organization for this token) is treated
// as gone (state removed) instead of an error that keeps state: once the 403
// stops, the PlanOnly step must see no drift.
func TestOrganizationResource_Read403IsAnErrorAndKeepsState(t *testing.T) {
	api := setupFake(t)
	route := "GET /organizations/" + testOrg
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			importOrgStep(nil),
			{
				PreConfig: func() {
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "forbidden", "message": "no access to this organization for this token"}})
					}
				},
				Config:      orgHCL(),
				ExpectError: loose("no access to this organization for this token"),
			},
			{
				PreConfig: func() { delete(api.Handlers, route) },
				Config:    orgHCL(), PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// ---------------------------------------------------------------- 10

// NOTE (review r1 M-7): this exercises a FAKE-ONLY path. The real API answers
// 403 - never 404 - to GET /organizations/{id} for any id other than the
// token's own, foreign or unknown alike (contract §1). The 404 branch of Read
// is defensive code the plan asks for (T9-8); do not cite this test as
// evidence about API behaviour.
// Fails if a 404 on Read leaves the resource in state (the plan would be
// empty) or is turned into an error (the step would fail outright). A
// permanent 404 route is safe here because the step is PlanOnly and the
// teardown destroy is state-only.
func TestOrganizationResource_Read404RemovesTheResourceFromState(t *testing.T) {
	api := setupFake(t)
	route := "GET /organizations/" + testOrg
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			importOrgStep(nil),
			{
				PreConfig: func() {
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "organization not found"}})
					}
				},
				Config: orgHCL(), PlanOnly: true, ExpectNonEmptyPlan: true,
			},
			{PreConfig: func() { delete(api.Handlers, route) }, Config: providerOnlyHCL},
		},
	})
}

// checkAbsentFromState fails if addr is still in state. (TestCheckNoResourceAttr
// cannot be used after a destroy: it errors "Not found" on an absent resource.)
func checkAbsentFromState(addr string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		if _, ok := s.RootModule().Resources[addr]; ok {
			return fmt.Errorf("%s is still in state after destroy", addr)
		}
		return nil
	}
}
