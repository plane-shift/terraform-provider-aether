package provider_test

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive aether_billing_budget (internal/provider/resource_billing_budget.go,
// registered in provider.go as NewBillingBudgetResource) through a real
// terraform (or tofu) binary against the in-process fake (fakeapi/budgets.go),
// under the controller rulings T12-1..T12-7 of task-12-brief-amended.md.
// Tests 1-10 are the amended brief's numbered list (see the numbered section
// markers below); task-12-tests-report.md records, per test, exactly how it
// is expected to fail against both no implementation and a plausible wrong
// one.

const (
	budgetResource = "aether_billing_budget.b"
	budgetRE       = `/billing/budget$`
)

// budgetHCL renders one aether_billing_budget resource. limit is rendered
// as a raw HCL numeric literal (not quoted), so a test can pass exactly the
// text it wants evaluated (e.g. "1e10", "12.345", "9999999999.99") rather
// than a Go-formatted float that might silently normalise it first.
// thresholds_pct is Required, so every config renders it explicitly - nil
// renders `[]`, which the SizeAtLeast(1) validator must refuse at plan time,
// never an omitted attribute.
func budgetHCL(limit string, thresholds []int64, extra ...string) string {
	th := make([]string, len(thresholds))
	for i, t := range thresholds {
		th[i] = strconv.FormatInt(t, 10)
	}
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_billing_budget" "b" {
  monthly_limit_eur = %s
  thresholds_pct    = [%s]
%s
}
`, limit, strings.Join(th, ", "), strings.Join(extra, "\n"))
}

// budgetAttrs is a shorthand for the resource's flat state attribute map.
func budgetAttrs(s *terraform.State) map[string]string { return attrs(s, budgetResource) }

// ---------------------------------------------------------------- 1

// Fails if Create does not GET first (T12-4: an extra or a missing GET), if
// it sends more than one PUT, if the PUT body carries anything but exactly
// {monthly_limit_eur, thresholds_pct} in that key order and those exact
// values, or if id/created_at/updated_at are not set in the resulting
// state. Also fails against a wrong implementation that PUTs unconditionally
// without checking for a pre-existing budget first (no way to tell this
// test alone from test 2's "GET already found something" case, but the GET
// count here pins that a GET happens at all before any PUT).
func TestBillingBudgetResource_CreateGetsFirstThenPutsExactBody(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: budgetHCL("100", []int64{50, 80, 100}),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(budgetResource, "id", testOrg),
				resource.TestCheckResourceAttr(budgetResource, "monthly_limit_eur", "100"),
				resource.TestCheckResourceAttr(budgetResource, "thresholds_pct.#", "3"),
				resource.TestCheckResourceAttr(budgetResource, "thresholds_pct.0", "50"),
				resource.TestCheckResourceAttr(budgetResource, "thresholds_pct.1", "80"),
				resource.TestCheckResourceAttr(budgetResource, "thresholds_pct.2", "100"),
				resource.TestCheckResourceAttrSet(budgetResource, "created_at"),
				resource.TestCheckResourceAttrSet(budgetResource, "updated_at"),
				func(*terraform.State) error {
					gets := requestsMatching(api, http.MethodGet, budgetRE)
					if len(gets) != 1 {
						return fmt.Errorf("want exactly one GET (the pre-create existence check, T12-4), got %d: %v", len(gets), gets)
					}
					puts := requestsMatching(api, http.MethodPut, budgetRE)
					if len(puts) != 1 {
						return fmt.Errorf("want exactly one PUT, got %d: %v", len(puts), puts)
					}
					if got, want := compactJSON(t, puts[0].Body), `{"monthly_limit_eur":100,"thresholds_pct":[50,80,100]}`; got != want {
						return fmt.Errorf("PUT body = %s, want exactly %s", got, want)
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- 2

// Fails if Create silently overwrites the pre-existing budget (any PUT at
// all), if the error does not mention import, or if anything is left in
// state despite the error (checked indirectly: a config with the resource
// removed must show an EMPTY plan afterwards - if Create had saved
// anything, removing it from config would plan a destroy instead).
func TestBillingBudgetResource_CreateWhenABudgetAlreadyExistsErrorsWithImportHintAndZeroPUTs(t *testing.T) {
	api := setupFake(t)
	api.SeedBudget(testOrg, 50, []int{25, 50})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config:      budgetHCL("100", []int64{50}),
				ExpectError: loose("import it"),
			},
			{
				// If Create had saved state before erroring, this config (the
				// resource removed entirely) would plan a non-empty destroy.
				Config: providerOnlyHCL, PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
	if puts := requestsMatching(api, http.MethodPut, budgetRE); len(puts) != 0 {
		t.Fatalf("create-when-a-budget-already-exists still sent a PUT: %v", puts)
	}
	if gets := requestsMatching(api, http.MethodGet, budgetRE); len(gets) == 0 {
		t.Fatalf("create-when-a-budget-already-exists never GET first")
	}
}

// ---------------------------------------------------------------- 3

// Fails if Update sends a partial body (missing either field - the API has
// no partial merge, so a thresholds-only PUT after a limit-only config
// change would be rejected as `monthly_limit_eur`/invalid on the real API),
// if updated_at does not change, if created_at DOES change, or if the
// pre-apply plan fails to show updated_at as unknown (T12-5).
func TestBillingBudgetResource_UpdateLimitOnlySendsBothFieldsAndBumpsUpdatedAt(t *testing.T) {
	api := setupFake(t)
	var firstCreated, firstUpdated string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: budgetHCL("100", []int64{50, 80, 100}),
				Check: func(s *terraform.State) error {
					a := budgetAttrs(s)
					firstCreated, firstUpdated = a["created_at"], a["updated_at"]
					if firstCreated == "" || firstUpdated == "" {
						return fmt.Errorf("created_at/updated_at not set after create: %q/%q", firstCreated, firstUpdated)
					}
					return nil
				},
			},
			{
				Config: budgetHCL("200", []int64{50, 80, 100}),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectUnknownValue(budgetResource, tfjsonpath.New("updated_at")),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(budgetResource, "monthly_limit_eur", "200"),
					func(s *terraform.State) error {
						a := budgetAttrs(s)
						if a["created_at"] != firstCreated {
							return fmt.Errorf("created_at changed on update: got %q, want unchanged %q", a["created_at"], firstCreated)
						}
						if a["updated_at"] == firstUpdated {
							return fmt.Errorf("updated_at did not change on update")
						}
						puts := requestsMatching(api, http.MethodPut, budgetRE)
						if len(puts) != 2 {
							return fmt.Errorf("want exactly 2 PUTs total (create + update), got %d: %v", len(puts), puts)
						}
						last := puts[len(puts)-1]
						if got, want := compactJSON(t, last.Body), `{"monthly_limit_eur":200,"thresholds_pct":[50,80,100]}`; got != want {
							return fmt.Errorf("update PUT body = %s, want exactly %s (full replace, both fields)", got, want)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 4

// Fails if a no-op second apply (identical config) sends any PUT at all -
// the refresh's own GET is expected and excluded from the assertion.
func TestBillingBudgetResource_SecondApplyWithIdenticalConfigSendsZeroPUTs(t *testing.T) {
	api := setupFake(t)
	cfg := budgetHCL("100", []int64{50, 80, 100})
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				// A real second apply (not PlanOnly): otherwise no PUT could ever
				// be sent by this step regardless of the resource's behaviour, and
				// the zero-PUT assertion below could never fail.
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
				Check: func(*terraform.State) error {
					var gets, puts int
					for _, r := range requestsSince(api, seen) {
						switch r.Method {
						case http.MethodGet:
							gets++
						case http.MethodPut:
							puts++
						}
					}
					if puts != 0 {
						return fmt.Errorf("second apply with identical config sent %d PUT(s), want 0", puts)
					}
					if gets == 0 {
						return fmt.Errorf("second apply with identical config sent zero GETs; want the refresh's own GET (otherwise this test cannot tell 'correctly did nothing' from 'never ran at all')")
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- 5

// Fails if any plan-time validator is missing or too permissive: each case
// must be refused WITHOUT a single request reaching the budget route.
func TestBillingBudgetResource_PlanTimeValidationRefusesWithZeroRequests(t *testing.T) {
	cases := []struct {
		name       string
		limit      string
		thresholds []int64
		expect     *regexp.Regexp
	}{
		{"limit zero", "0", []int64{50}, loose("must be at least 0.01")},
		{"limit below the one-cent floor", "0.001", []int64{50}, loose("must be at least 0.01")},
		{"limit three decimal places", "12.345", []int64{50}, loose("more than two decimal places")},
		{"limit at the 1e10 ceiling", "1e10", []int64{50}, loose("must be less than 10000000000")},
		{"thresholds empty", "100", nil, loose("list must contain at least 1 elements")},
		{"threshold zero", "100", []int64{0}, loose("value must be between 1 and 100")},
		{"threshold above 100", "100", []int64{101}, loose("value must be between 1 and 100")},
		{"thresholds not ascending", "100", []int64{80, 50}, loose("thresholds must be strictly ascending")},
		{"thresholds duplicate", "100", []int64{50, 50}, loose("thresholds must be strictly ascending")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{{
					Config:      budgetHCL(c.limit, c.thresholds),
					ExpectError: c.expect,
				}},
			})
			if reqs := requestsMatching(api, http.MethodGet, budgetRE); len(reqs) != 0 {
				t.Fatalf("%s: a plan-time validation failure reached the API with a GET: %v", c.name, reqs)
			}
			if reqs := requestsMatching(api, http.MethodPut, budgetRE); len(reqs) != 0 {
				t.Fatalf("%s: a plan-time validation failure reached the API with a PUT: %v", c.name, reqs)
			}
		})
	}
}

// ---------------------------------------------------------------- 6

// Fails if the plan-time decimal-places/round-trip check is too strict for
// values that ARE at most 2 decimal places (a false plan-time refusal), or
// if the apply produces "Provider produced inconsistent result after apply"
// for any of them (U1: a float64 NUMERIC(12,2) round-trip mismatch) - that
// failure would surface as the whole resource.UnitTest call failing, not as
// a normal assertion, so there is nothing further to check in that case;
// the state-value assertion below only runs at all once apply already
// succeeded cleanly.
func TestBillingBudgetResource_LimitRoundTripsExactlyForCommonValues(t *testing.T) {
	for _, limit := range []string{"12.3", "0.01", "9999999999.99"} {
		t.Run(limit, func(t *testing.T) {
			setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{{
					Config: budgetHCL(limit, []int64{50}),
					Check:  resource.TestCheckResourceAttr(budgetResource, "monthly_limit_eur", limit),
				}},
			})
		})
	}
}

// ---------------------------------------------------------------- 7

// Fails if import does not adopt every GET-derived field, if it ever PUTs
// (adopt-only), or if importing when no budget is set succeeds instead of
// erroring with T12-1's exact message.
func TestBillingBudgetResource_ImportByOrgIDReadsState(t *testing.T) {
	api := setupFake(t)
	api.SeedBudget(testOrg, 75, []int{25, 50, 75})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName: budgetResource, ImportState: true, ImportStatePersist: true,
			Config:        budgetHCL("75", []int64{25, 50, 75}),
			ImportStateId: testOrg,
			ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 {
					return fmt.Errorf("want exactly one imported instance, got %d", len(is))
				}
				a := is[0].Attributes
				if a["id"] != testOrg {
					return fmt.Errorf("imported id = %q, want the org id %q", a["id"], testOrg)
				}
				if a["monthly_limit_eur"] != "75" {
					return fmt.Errorf("imported monthly_limit_eur = %q, want %q", a["monthly_limit_eur"], "75")
				}
				if a["thresholds_pct.#"] != "3" || a["thresholds_pct.0"] != "25" || a["thresholds_pct.1"] != "50" || a["thresholds_pct.2"] != "75" {
					return fmt.Errorf("imported thresholds_pct = %v, want [25,50,75] in order", a)
				}
				for _, k := range []string{"created_at", "updated_at"} {
					if a[k] == "" {
						return fmt.Errorf("imported %s is empty, want the GET-derived timestamp", k)
					}
				}
				if reqs := requestsMatching(api, http.MethodPut, budgetRE); len(reqs) != 0 {
					return fmt.Errorf("import must never PUT (adopt-only), got %d: %v", len(reqs), reqs)
				}
				return nil
			},
		}},
	})
}

// Fails if importing when no budget is set either succeeds (adopting
// nothing) or errors with different wording than T12-1's exact message.
func TestBillingBudgetResource_ImportWhenNoBudgetIsSetErrors(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName: budgetResource, ImportState: true,
			Config:        budgetHCL("50", []int64{50}),
			ImportStateId: testOrg,
			ExpectError:   loose("no budget is set for this organization"),
		}},
	})
}

// ---------------------------------------------------------------- 8

// Fails if a 404 on Read (the fake's RemoveBudget, applied out of band in
// the NEXT step's PreConfig - removing it any earlier would just make the
// CREATE step itself never happen) does not remove the resource from state,
// if the next apply does not plan a Create (not an Update), or if it does
// not send exactly the original create PUT plus one PUT for the recreate.
// Create's GET-before-PUT ordering (T12-4) applies identically to a
// recreate, but is pinned by tests 1 and 2, not asserted here.
func TestBillingBudgetResource_Read404RemovesFromStateAndNextApplyRecreates(t *testing.T) {
	api := setupFake(t)
	cfg := budgetHCL("100", []int64{50})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() { api.RemoveBudget(testOrg) },
				Config:    cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(budgetResource, plancheck.ResourceActionCreate),
				}},
				Check: func(*terraform.State) error {
					puts := requestsMatching(api, http.MethodPut, budgetRE)
					if len(puts) != 2 {
						return fmt.Errorf("want the original create PUT plus the recreate's PUT, got %d: %v", len(puts), puts)
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- 9

// Fails if destroy sends anything other than exactly one DELETE.
func TestBillingBudgetResource_DestroySendsExactlyOneDelete(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: budgetHCL("100", []int64{50})},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(budgetResource),
					func(*terraform.State) error {
						var n int
						for _, r := range requestsSince(api, seen) {
							if r.Method == http.MethodDelete {
								n++
							}
						}
						if n != 1 {
							return fmt.Errorf("destroy sent %d DELETE(s), want exactly 1", n)
						}
						return nil
					},
				),
			},
		},
	})
}

// Fails if Delete treats the API's 404 (already removed out of band, on the
// DELETE request itself) as an error: the destroy must succeed, leave state
// empty, and still have sent the DELETE (a Delete that sends nothing would
// otherwise also leave state empty and pass).
func TestBillingBudgetResource_DeleteOfAnAlreadyRemovedBudgetIsSuccess(t *testing.T) {
	api := setupFake(t)
	route := "DELETE /organizations/" + testOrg + "/billing/budget"
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: budgetHCL("100", []int64{50})},
			{
				// The refresh sees the budget (so Delete runs); the DELETE call
				// itself removes it out of band FIRST and then answers 404 -
				// exactly a removal between the refresh and the delete.
				PreConfig: func() {
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						api.RemoveBudget(testOrg)
						fakeapi.WriteJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "No budget set"}})
					}
				},
				Config: providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(budgetResource),
					func(*terraform.State) error {
						// The override runs after the request is recorded
						// (fakeapi.go's serve dispatch), so this count includes it.
						if reqs := requestsMatching(api, http.MethodDelete, budgetRE); len(reqs) == 0 {
							return fmt.Errorf("delete-of-an-already-removed-budget sent zero DELETEs")
						}
						return nil
					},
				),
			},
		},
	})
}

// Fails if a persistent DELETE failure (403, not a 5xx - transport/retry.go
// retries a DELETE 5xx, which a 403 avoids) is swallowed instead of
// surfacing the API's message, or if any state is lost despite
// the failure (checked by reapplying the ORIGINAL config afterwards and
// requiring an empty plan - if Delete had cleared state anyway, that would
// plan a create instead).
func TestBillingBudgetResource_DeleteFailureSurfacesErrorAndKeepsState(t *testing.T) {
	api := setupFake(t)
	route := "DELETE /organizations/" + testOrg + "/billing/budget"
	cfg := budgetHCL("100", []int64{50})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "forbidden", "message": "delete budget boom"}})
					}
				},
				Config:      providerOnlyHCL,
				ExpectError: loose("delete budget boom"),
			},
			{
				PreConfig: func() { delete(api.Handlers, route) },
				Config:    cfg, PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// ---------------------------------------------------------------- 10

// Fails if a 422 from the API (forced via a Handlers override - the
// plan-time validators make a real ascending-order 422 unreachable through
// normal config, since the same rule is enforced at plan time) is swallowed
// or reworded instead of surfacing the API's own detail message verbatim.
func TestBillingBudgetResource_APIValidationErrorSurfacesExactDetailMessage(t *testing.T) {
	api := setupFake(t)
	route := "PUT /organizations/" + testOrg + "/billing/budget"
	api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
		fakeapi.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{
				"code":    "validation_error",
				"message": "Request validation failed",
				"details": []map[string]any{
					{"field": "thresholds_pct", "code": "invalid", "message": "thresholds must be strictly ascending"},
				},
			},
		})
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      budgetHCL("100", []int64{50, 80, 100}),
			ExpectError: loose("thresholds must be strictly ascending"),
		}},
	})
	if reqs := requestsMatching(api, http.MethodGet, budgetRE); len(reqs) != 1 {
		t.Fatalf("want exactly one GET (the pre-create check) before the overridden PUT surfaced its error, got %d: %v", len(reqs), reqs)
	}
}
