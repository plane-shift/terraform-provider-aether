package provider_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive aether_registry_cleanup_policy
// (internal/provider/resource_registry_cleanup_policy.go, registered in
// provider.go's Resources() as NewRegistryCleanupPolicyResource) through a
// real terraform (or tofu) binary against the in-process fake
// (fakeapi/registry.go), under the controller rulings T11-9..T11-14 of
// task-11-brief-amended.md. testOrg's slug is "acme" (setupFake).
//
// Body assertions here use jsonBodyDiff (structural, key-ORDER-agnostic)
// rather than the compactJSON byte-exact idiom the other two files in this
// task use: aether-api's generated CleanupCondition.Op is a plain
// (non-pointer, non-omitempty) string (client.gen.go), so a provider that
// must OMIT `op`/`value` for an untagged condition with no op (T11-11) cannot
// marshal the generated type directly for a per-condition body and is free
// to choose its own field order for the request as a whole. jsonBodyDiff
// still catches every real defect (extra/missing/wrong-value keys, and -
// critically - a key that must be ABSENT vs. present-as-null/""), just not a
// coincidental reordering.
//
// Every test here fails against the current (unimplemented) provider with
// terraform-plugin-framework's "Invalid Resource Type"/"Resource Type Not
// Found" diagnostic - the expected RED state.

const (
	cleanupPolicyResource = "aether_registry_cleanup_policy.c"
	policiesListRE        = `/registry/cleanup-policies$`
	policyOneRE           = `/registry/cleanup-policies/[^/]+$`
)

// conditionHCL renders one conditions list element, omitting any key not
// present in c (Optional op/value must be omittable in config).
func conditionHCL(c map[string]string) string {
	var parts []string
	for _, k := range []string{"type", "op", "value"} {
		if v, ok := c[k]; ok {
			parts = append(parts, fmt.Sprintf("%s = %q", k, v))
		}
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// cleanupPolicyHCL renders the cleanup policy under test; extra lines go
// inside the block (name and conditions are the only Required attributes).
func cleanupPolicyHCL(name string, conditions []map[string]string, extra ...string) string {
	condParts := make([]string, len(conditions))
	for i, c := range conditions {
		condParts[i] = conditionHCL(c)
	}
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_registry_cleanup_policy" "c" {
  name       = %q
  conditions = [%s]
%s
}
`, name, strings.Join(condParts, ", "), strings.Join(extra, "\n"))
}

// jsonBodyDiff decodes body and compares it structurally (not by key order)
// against want - see the file doc comment for why this file uses structural
// comparison instead of the project's usual byte-exact compactJSON.
func jsonBodyDiff(body []byte, want map[string]any) error {
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		return fmt.Errorf("body not JSON: %q: %v", body, err)
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("body = %#v,\nwant  %#v", got, want)
	}
	return nil
}

func noCleanupPolicyRequests(t *testing.T, api *fakeapi.Server) {
	t.Helper()
	for _, r := range api.Requests() {
		if strings.Contains(r.Path, "/cleanup-policies") {
			t.Fatalf("a cleanup-policies API call was made: %s %s", r.Method, r.Path)
		}
	}
}

// deleteThenListHappened reports whether, among the requests recorded since
// seen, at least one cleanup-policy DELETE was sent, and whether at least one
// cleanup-policies list GET came AFTER the last such DELETE - the T11-13
// re-list Delete must perform on any non-204 (I-2: both the "already gone"
// and the "genuine 500" destroy tests need this, not just a bare DELETE
// count, since the re-list is the whole point of the branch under test).
func deleteThenListHappened(api *fakeapi.Server, seen int) (deleted, listAfterDelete bool) {
	return deleteThenListHappenedInWindow(api, seen, len(api.Requests()))
}

// deleteThenListHappenedInWindow is deleteThenListHappened bounded to
// api.Requests()[seen:end] instead of running to the CURRENT length: a
// caller whose own TestStep issues further requests after the window it
// means to inspect (e.g. a RefreshState step's own state-refresh GET, which
// lands after a DELETE sent by a PRIOR step) must capture end before those
// extra requests happen, or "a list GET after the DELETE" is trivially true
// regardless of whether the code under test performed it (m-3e).
func deleteThenListHappenedInWindow(api *fakeapi.Server, seen, end int) (deleted, listAfterDelete bool) {
	oneRE, listRE := regexp.MustCompile(policyOneRE), regexp.MustCompile(policiesListRE)
	deleteAt, listAfterDeleteAt := -1, -1
	for i, r := range api.Requests()[seen:end] {
		if r.Method == http.MethodDelete && oneRE.MatchString(r.Path) {
			deleteAt = i
		}
		if deleteAt >= 0 && listAfterDeleteAt < 0 && r.Method == http.MethodGet && listRE.MatchString(r.Path) {
			listAfterDeleteAt = i
		}
	}
	return deleteAt >= 0, listAfterDeleteAt >= 0
}

// ---------------------------------------------------------------- C1

// Fails if create sends anything but every field of the minimal body (an
// omitted action/schedule/repo_scope would 500 against the real API, §9), or
// if last_run_* are anything but null on a freshly created policy.
func TestCleanupPolicyResource_CreateMinimalSendsFullBody(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(cleanupPolicyResource, "name", "p"),
				resource.TestCheckResourceAttr(cleanupPolicyResource, "action", "delete"),
				resource.TestCheckResourceAttr(cleanupPolicyResource, "schedule", "nightly"),
				resource.TestCheckResourceAttr(cleanupPolicyResource, "enabled", "true"),
				resource.TestCheckResourceAttr(cleanupPolicyResource, "repo_scope.#", "0"),
				resource.TestCheckResourceAttr(cleanupPolicyResource, "conditions.#", "1"),
				resource.TestCheckNoResourceAttr(cleanupPolicyResource, "last_run_at"),
				resource.TestCheckNoResourceAttr(cleanupPolicyResource, "last_run_deleted"),
				resource.TestCheckNoResourceAttr(cleanupPolicyResource, "last_run_freed_bytes"),
				func(*terraform.State) error {
					posts := requestsMatching(api, http.MethodPost, policiesListRE)
					if len(posts) != 1 {
						return fmt.Errorf("want exactly one POST, got %d: %v", len(posts), posts)
					}
					return jsonBodyDiff(posts[0].Body, map[string]any{
						"name": "p", "action": "delete", "schedule": "nightly", "enabled": true,
						"repo_scope": []any{},
						"conditions": []any{map[string]any{"type": "age", "op": "gt", "value": "30d"}},
					})
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- C2

// Fails if an untagged condition with an explicit boolean-string value sends
// an `op` key at all - T11-11 requires op/value be OMITTED from the JSON
// when null, and untagged's op is always null (T11-10).
func TestCleanupPolicyResource_UntaggedConditionOmitsOpFromBody(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: cleanupPolicyHCL("p", []map[string]string{{"type": "untagged", "value": "true"}}),
			Check: func(*terraform.State) error {
				posts := requestsMatching(api, http.MethodPost, policiesListRE)
				if len(posts) != 1 {
					return fmt.Errorf("want exactly one POST, got %d", len(posts))
				}
				return jsonBodyDiff(posts[0].Body, map[string]any{
					"name": "p", "action": "delete", "schedule": "nightly", "enabled": true,
					"repo_scope": []any{},
					"conditions": []any{map[string]any{"type": "untagged", "value": "true"}}, // no "op" key
				})
			},
		}},
	})
}

// Fails if an untagged condition with no `value` is accepted: the engine
// treats a missing/unparseable value as false, matching EVERY tagged
// artifact - a fail-OPEN the provider must refuse at plan time (T11-10).
func TestCleanupPolicyResource_UntaggedWithoutValueIsPlanTimeError(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      cleanupPolicyHCL("p", []map[string]string{{"type": "untagged"}}),
			ExpectError: loose("value"),
		}},
	})
	noCleanupPolicyRequests(t, api)
}

// Fails if an untagged condition carrying an `op` is accepted (T11-10: op
// must be null for untagged).
func TestCleanupPolicyResource_UntaggedWithOpIsPlanTimeError(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      cleanupPolicyHCL("p", []map[string]string{{"type": "untagged", "op": "gt", "value": "true"}}),
			ExpectError: loose("op"),
		}},
	})
	noCleanupPolicyRequests(t, api)
}

// ---------------------------------------------------------------- C3

// Fails if any per-type condition validator is missing or too permissive:
// each case must be refused WITHOUT a single request reaching the
// cleanup-policies API (T11-10).
func TestCleanupPolicyResource_PerTypeConditionValidatorsRefuseAtPlanTime(t *testing.T) {
	cases := []struct {
		name      string
		condition map[string]string
	}{
		{"age bad value", map[string]string{"type": "age", "op": "gt", "value": "30"}},                // not "<N>d" or a Go duration
		{"tag_count bad op", map[string]string{"type": "tag_count", "op": "lt", "value": "5"}},        // only gt is valid
		{"tag_name bad regexp", map[string]string{"type": "tag_name", "op": "matches", "value": "["}}, // uncompilable
		{"unknown type", map[string]string{"type": "bogus", "op": "gt", "value": "1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps: []resource.TestStep{{
					Config:      cleanupPolicyHCL("p", []map[string]string{c.condition}),
					ExpectError: loose(c.condition["type"]),
				}},
			})
			noCleanupPolicyRequests(t, api)
		})
	}
}

// ---------------------------------------------------------------- C4

// Fails if changing ONLY `enabled` sends a partial PUT: the API's PUT is a
// full replace of every field but enabled (T11-11), so the provider must
// always send name/action/conditions/repo_scope/schedule too.
func TestCleanupPolicyResource_UpdateEnabledOnlySendsFullPUTBody(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}})},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}, "enabled = false"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(cleanupPolicyResource, "enabled", "false"),
					func(*terraform.State) error {
						puts := filterReqs(requestsSince(api, seen), http.MethodPut, policyOneRE)
						if len(puts) != 1 {
							return fmt.Errorf("want exactly one PUT, got %d: %v", len(puts), puts)
						}
						return jsonBodyDiff(puts[0].Body, map[string]any{
							"name": "p", "action": "delete", "schedule": "nightly", "enabled": false,
							"repo_scope": []any{},
							"conditions": []any{map[string]any{"type": "age", "op": "gt", "value": "30d"}},
						})
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- C5

// Fails if changing conditions does not PUT the full replacement, or if the
// resulting state does not reflect the new conditions.
func TestCleanupPolicyResource_UpdateConditionsSendsFullPUTAndStateMatches(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}})},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    cleanupPolicyHCL("p", []map[string]string{{"type": "tag_count", "op": "gt", "value": "5"}}),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(cleanupPolicyResource, "conditions.#", "1"),
					resource.TestCheckResourceAttr(cleanupPolicyResource, "conditions.0.type", "tag_count"),
					resource.TestCheckResourceAttr(cleanupPolicyResource, "conditions.0.value", "5"),
					func(*terraform.State) error {
						puts := filterReqs(requestsSince(api, seen), http.MethodPut, policyOneRE)
						if len(puts) != 1 {
							return fmt.Errorf("want exactly one PUT, got %d: %v", len(puts), puts)
						}
						return jsonBodyDiff(puts[0].Body, map[string]any{
							"name": "p", "action": "delete", "schedule": "nightly", "enabled": true,
							"repo_scope": []any{},
							"conditions": []any{map[string]any{"type": "tag_count", "op": "gt", "value": "5"}},
						})
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- C6

// Fails if Read does not stringify a numeric condition value written by
// another client (float64 10 -> "10"), or if it does not read the portal's
// fail-open shape ({"type":"untagged"}, no op/value) back as null op/value
// with no error (§9 U3).
func TestCleanupPolicyResource_ReadNormalisesNumericValueAndPortalShape(t *testing.T) {
	api := setupFake(t)
	id := api.SeedPolicy(testOrg, "p", "delete", []map[string]any{
		{"type": "tag_count", "op": "gt", "value": float64(10)},
		{"type": "untagged"},
	}, []string{}, "nightly", true)

	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName:       cleanupPolicyResource,
			ImportState:        true,
			ImportStatePersist: true,
			// The untagged condition carries an explicit value in Config (the
			// ruled plan-time validator, T11-10, refuses a valueless untagged
			// condition) - the seeded policy's own {"type":"untagged"} (no
			// op/value at all) is what ImportStateCheck below inspects, via
			// the imported STATE, not this config.
			Config:        cleanupPolicyHCL("p", []map[string]string{{"type": "tag_count", "op": "gt", "value": "10"}, {"type": "untagged", "value": "true"}}),
			ImportStateId: id,
			ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 {
					return fmt.Errorf("want exactly one imported instance, got %d", len(is))
				}
				a := is[0].Attributes
				if a["conditions.0.value"] != "10" {
					return fmt.Errorf("conditions.0.value = %q, want the stringified number \"10\"", a["conditions.0.value"])
				}
				if a["conditions.1.type"] != "untagged" {
					return fmt.Errorf("conditions.1.type = %q, want untagged", a["conditions.1.type"])
				}
				for _, k := range []string{"conditions.1.op", "conditions.1.value"} {
					if v, has := a[k]; has {
						return fmt.Errorf("%s = %q, want null (the portal's fail-open shape has neither key)", k, v)
					}
				}
				return nil
			},
		}},
	})
}

// ---------------------------------------------------------------- C7

// Fails if Read does not remove a policy deleted out of band.
func TestCleanupPolicyResource_ReadRemovedOutOfBandRemovesFromState(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
				Check: func(s *terraform.State) error {
					id = attrs(s, cleanupPolicyResource)["id"]
					return nil
				},
			},
			{
				// The removal must happen between step 1's own post-apply
				// refresh-plan check (which still sees the policy present)
				// and step 2's apply - doing it inside step 1's Check would
				// make that automatic refresh-plan check itself see a
				// missing policy and fail with a non-empty plan (T11 D2).
				PreConfig: func() {
					if !api.RemovePolicy(testOrg, id) {
						t.Fatalf("RemovePolicy(%s) reported the policy missing", id)
					}
				},
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
				Check: func(*terraform.State) error {
					if posts := requestsMatching(api, http.MethodPost, policiesListRE); len(posts) != 2 {
						return fmt.Errorf("total POSTs = %d, want 2 (the original create plus the recreate)", len(posts))
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- C8

// Fails if import does not adopt every GET-derived field via the id-matching
// list read (there is no GET-by-id route, §10/T11-12), including the actual
// VALUES of repo_scope and conditions, not merely their counts (m-3b).
func TestCleanupPolicyResource_ImportByIDAdoptsFields(t *testing.T) {
	api := setupFake(t)
	id := api.SeedPolicy(testOrg, "p", "protect", []map[string]any{
		{"type": "tag_name", "op": "eq", "value": "latest"},
	}, []string{"foo/*"}, "weekly", false)

	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName:       cleanupPolicyResource,
			ImportState:        true,
			ImportStatePersist: true,
			Config:             cleanupPolicyHCL("p", []map[string]string{{"type": "tag_name", "op": "eq", "value": "latest"}}, `action = "protect"`, `schedule = "weekly"`, `enabled = false`, `repo_scope = ["foo/*"]`),
			ImportStateId:      id,
			ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 {
					return fmt.Errorf("want exactly one imported instance, got %d", len(is))
				}
				a := is[0].Attributes
				for k, want := range map[string]string{
					"id": id, "name": "p", "action": "protect", "schedule": "weekly", "enabled": "false",
					"repo_scope.#": "1", "conditions.#": "1",
					"conditions.0.type": "tag_name", "conditions.0.op": "eq", "conditions.0.value": "latest",
				} {
					if a[k] != want {
						return fmt.Errorf("imported %s = %q, want %q", k, a[k], want)
					}
				}
				if got, want := setValues(a, "repo_scope"), []string{"foo/*"}; !reflect.DeepEqual(got, want) {
					return fmt.Errorf("imported repo_scope = %v, want %v", got, want)
				}
				return nil
			},
		}},
	})
}

// ---------------------------------------------------------------- C9

// Fails if destroy sends anything but exactly one DELETE.
func TestCleanupPolicyResource_DestroySendsExactlyOneDelete(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}})},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(cleanupPolicyResource),
					func(*terraform.State) error {
						d := filterReqs(requestsSince(api, seen), http.MethodDelete, policyOneRE)
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

// Fails if Delete treats the fake's real "already gone" answer (500
// "cleanup policy not found" - the API never 404s here, T11-13) as an error
// instead of re-listing and finding the id absent: destroy must succeed. The
// removal happens INSIDE the DELETE route itself (a handler override
// installed in step 1's Check), not in step 2's PreConfig, so the destroy
// step's own refresh does NOT see the policy already gone and Delete's
// re-list branch actually runs (I-2: the original version removed the
// policy in PreConfig, so the refresh emptied the plan before apply and
// Delete was never called - a real "treat this 500 as an error" bug would
// still have passed 35/35 tests). Asserts a DELETE was sent AND a list GET
// came after it, not just that the resource ended up absent.
func TestCleanupPolicyResource_DestroyOfAnAlreadyRemovedPolicyIsSuccess(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
				Check: func(s *terraform.State) error {
					id := attrs(s, cleanupPolicyResource)["id"]
					route := "DELETE /organizations/" + testOrg + "/registry/cleanup-policies/" + id
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						api.RemovePolicy(testOrg, id)
						http.Error(w, "cleanup policy not found", http.StatusInternalServerError)
					}
					return nil
				},
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(cleanupPolicyResource),
					func(*terraform.State) error {
						deleted, listAfter := deleteThenListHappened(api, seen)
						if !deleted {
							return fmt.Errorf("no DELETE was sent, want at least one (§7/T11-13, I-2)")
						}
						if !listAfter {
							return fmt.Errorf("no list GET after the DELETE, want at least one (T11-13's re-list, I-2)")
						}
						return nil
					},
				),
			},
		},
	})
}

// Fails if Delete swallows a GENUINE 500 (the policy is still there) just
// because the status code matches the "already gone" shape: re-listing must
// find it present and error (T11-13). Also asserts the re-list itself
// happened (DELETE + a list GET after it), which the original version left
// unchecked (m-3e).
func TestCleanupPolicyResource_DestroyGenuine500WithPolicyStillPresentErrors(t *testing.T) {
	api := setupFake(t)
	var route string
	var seen, end int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
				Check: func(s *terraform.State) error {
					id := attrs(s, cleanupPolicyResource)["id"]
					route = "DELETE /organizations/" + testOrg + "/registry/cleanup-policies/" + id
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						http.Error(w, "internal_error: something else broke", http.StatusInternalServerError)
					}
					seen = len(api.Requests())
					return nil
				},
			},
			{
				Config:      providerOnlyHCL,
				ExpectError: loose("internal_error"),
			},
			{
				// end is captured here, BEFORE this step's own RefreshState
				// GET fires, so the request-log check below sees only
				// api.Requests()[seen:end] - the prior step's failed-destroy
				// attempt - and not this step's unrelated state refresh.
				// Without that bound, this step's own refresh GET always
				// lands after the prior DELETE and "a list GET after the
				// DELETE" would be trivially true even if Delete's re-list
				// branch never ran (m-3e).
				PreConfig: func() {
					delete(api.Handlers, route)
					end = len(api.Requests())
				},
				// The failed destroy must have kept the policy in state; a
				// post-refresh plan against a state that still has it (config on
				// disk is providerOnlyHCL) wants to destroy it again.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeTestCheckFunc(
					func(s *terraform.State) error {
						if _, ok := s.RootModule().Resources[cleanupPolicyResource]; !ok {
							return fmt.Errorf("%s missing after the failed destroy, want it KEPT", cleanupPolicyResource)
						}
						return nil
					},
					func(*terraform.State) error {
						deleted, listAfter := deleteThenListHappenedInWindow(api, seen, end)
						if !deleted {
							return fmt.Errorf("no DELETE was sent during the genuine-500 destroy, want at least one")
						}
						if !listAfter {
							return fmt.Errorf("no list GET after the DELETE during the genuine-500 destroy, want at least one (T11-13's re-list, I-2/m-3e)")
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- C10

// Fails if a scheduler/manual run recorded via SetLastRun is not picked up by
// a plain refresh, or if it disturbs any OTHER managed attribute.
func TestCleanupPolicyResource_LastRunDriftRefreshesCleanly(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
				Check: func(s *terraform.State) error {
					id = attrs(s, cleanupPolicyResource)["id"]
					return nil
				},
			},
			{
				PreConfig: func() {
					if !api.SetLastRun(testOrg, id, 4, 2048, "some artifacts skipped") {
						t.Fatalf("SetLastRun(%s) reported the policy missing", id)
					}
				},
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(cleanupPolicyResource, "last_run_deleted", "4"),
					resource.TestCheckResourceAttr(cleanupPolicyResource, "last_run_freed_bytes", "2048"),
					resource.TestCheckResourceAttrSet(cleanupPolicyResource, "last_run_at"),
					resource.TestCheckResourceAttr(cleanupPolicyResource, "name", "p"),
					resource.TestCheckResourceAttr(cleanupPolicyResource, "action", "delete"),
					resource.TestCheckResourceAttr(cleanupPolicyResource, "schedule", "nightly"),
					resource.TestCheckResourceAttr(cleanupPolicyResource, "enabled", "true"),
					func(*terraform.State) error {
						if p := requestsMatching(api, http.MethodPut, policyOneRE); len(p) != 0 {
							return fmt.Errorf("a last-run-only drift caused a PUT: %v, want zero writes (refresh only)", p)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- m-2/m-3(c)

// Fails if updating a managed attribute after a scheduler run moves
// last_run_at as a side effect of the PUT (the real API's PUT never touches
// last_run_at - only a run does, §11/§12; m-2, mirrors
// TestCleanupPutAfterRunKeepsLastRunAt at the fake level).
func TestCleanupPolicyResource_UpdateAfterARunKeepsLastRunAt(t *testing.T) {
	api := setupFake(t)
	var id, before string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
				Check: func(s *terraform.State) error {
					id = attrs(s, cleanupPolicyResource)["id"]
					return nil
				},
			},
			{
				// Unchanged config: a plain refresh that picks up SetLastRun,
				// so `before` reflects what the provider itself read.
				PreConfig: func() {
					if !api.SetLastRun(testOrg, id, 4, 2048, "some artifacts skipped") {
						t.Fatalf("SetLastRun(%s) reported the policy missing", id)
					}
				},
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}),
				Check: func(s *terraform.State) error {
					before = attrs(s, cleanupPolicyResource)["last_run_at"]
					if before == "" {
						return fmt.Errorf("last_run_at empty after the refresh that should have picked up SetLastRun")
					}
					return nil
				},
			},
			{
				Config: cleanupPolicyHCL("p", []map[string]string{{"type": "age", "op": "gt", "value": "30d"}}, "enabled = false"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(cleanupPolicyResource, "enabled", "false"),
					func(s *terraform.State) error {
						after := attrs(s, cleanupPolicyResource)["last_run_at"]
						if after != before {
							return fmt.Errorf("last_run_at moved after an update (%q -> %q), want unchanged (only a run moves it, §11/§12, m-2)", before, after)
						}
						return nil
					},
				),
			},
		},
	})
}

// Fails if Read does not stringify a BOOLEAN condition value (true ->
// "true"), distinct from C6's numeric-value and portal-shape coverage
// (m-3c).
func TestCleanupPolicyResource_ReadNormalisesBooleanValue(t *testing.T) {
	api := setupFake(t)
	id := api.SeedPolicy(testOrg, "p", "delete", []map[string]any{
		{"type": "untagged", "value": true},
	}, []string{}, "nightly", true)

	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			ResourceName:       cleanupPolicyResource,
			ImportState:        true,
			ImportStatePersist: true,
			// The ruled plan-time validator (T11-10) refuses a valueless
			// untagged condition, so Config must carry an explicit value -
			// ImportStateCheck below inspects the imported STATE (the
			// seeded policy's boolean `true`), not this config.
			Config:        cleanupPolicyHCL("p", []map[string]string{{"type": "untagged", "value": "true"}}),
			ImportStateId: id,
			ImportStateCheck: func(is []*terraform.InstanceState) error {
				if len(is) != 1 {
					return fmt.Errorf("want exactly one imported instance, got %d", len(is))
				}
				a := is[0].Attributes
				if a["conditions.0.value"] != "true" {
					return fmt.Errorf("conditions.0.value = %q, want the stringified boolean \"true\"", a["conditions.0.value"])
				}
				return nil
			},
		}},
	})
}
