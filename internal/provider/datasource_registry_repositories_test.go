package provider_test

// Tests for the aether_registry_repositories data source (not yet
// implemented - datasource_registry_repositories.go, T14-4). Until
// NewRegistryRepositoriesDataSource is registered in provider.go's
// DataSources(), every test here fails at plan time with "Invalid Data
// Source Type" (see datasource_organization_members_test.go's header
// comment).

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const repositoriesDataSource = "data.aether_registry_repositories.r"

const repositoriesDSHCL = `
provider "aether" {}
data "aether_registry_repositories" "r" {}
`

// RP1. Seeds three repositories, forces the fake to serve them 2-per-page
// via SetHarborPageCap (independent of whatever limit the provider itself
// requests, per repositories.go's own doc comment), and checks: every
// attribute of one item (name/artifact_count - named honestly from
// tags_count, not "tags_count" itself - /size_bytes/last_push), the final
// list SORTED BY NAME (not insertion order: seeded z,a,m), and that the
// provider followed the fake's Harbor-page-number next_cursor "2" on its
// second request rather than stopping after page 1.
func TestRegistryRepositoriesDataSource_TwoPagesSortedByName(t *testing.T) {
	api := setupFake(t)
	push := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	api.SeedRepository(testOrg, "zeta", 3, 4096, &push)
	api.SeedRepository(testOrg, "alpha", 9, 0, nil)
	api.SeedRepository(testOrg, "mid", 1, 1024, nil)
	api.SetHarborPageCap(testOrg, 2)

	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: repositoriesDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.#", "3"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.0.name", "alpha"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.0.artifact_count", "9"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.0.size_bytes", "0"),
				resource.TestCheckNoResourceAttr(repositoriesDataSource, "repositories.0.last_push"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.1.name", "mid"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.2.name", "zeta"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.2.artifact_count", "3"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.2.size_bytes", "4096"),
				resource.TestCheckResourceAttrSet(repositoriesDataSource, "repositories.2.last_push"),
				checkAbsentFromStateSubstring(repositoriesDataSource, "vulnerabilities"),
				func(*terraform.State) error {
					// M-7: exactly 2, not merely ">= 2" - a 3-item, 2-per-page
					// listing ends with page 2's next_cursor "" (page 1 has 2
					// items + next_cursor "2", page 2 has the remaining 1 item +
					// next_cursor ""). The looser ">=2" bound could not tell
					// "stopped on the empty cursor" apart from "kept walking
					// past it" (collectAll looping until an error or timeout).
					gets := requestsMatching(api, http.MethodGet, `/registry/repositories$`)
					if len(gets) != 2 {
						return fmt.Errorf("GET repositories calls = %d, want exactly 2 (a 3-item, 2-per-page Harbor listing that stops on next_cursor==\"\")", len(gets))
					}
					if !hasQueryPair(gets[1].Query, "cursor", "2") {
						return fmt.Errorf("second GET query = %q, want the provider to have followed page 1's next_cursor=2", gets[1].Query)
					}
					return nil
				},
			),
		}},
	})
}

// hasQueryPair reports whether raw (a URL RawQuery) contains key=value.
func hasQueryPair(raw, key, value string) bool {
	v, err := url.ParseQuery(raw)
	if err != nil {
		return false
	}
	return v.Get(key) == value
}

// RP2. A never-enabled registry's repositories list is a 502 harbor_error on
// the wire; the data source must surface it as a plan-time error, never
// `200 []` (T14-4 description, contract §1).
func TestRegistryRepositoriesDataSource_NeverEnabledErrors(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      repositoriesDSHCL,
			ExpectError: loose("harbor_error"),
		}},
	})
}

// T14-15 (RED until Task 13's collectAll fix lands here at rebase - see
// task-14-tests-report.md "Fix round 1"): a Harbor page pager that answers
// the SAME next_cursor on every page is a broken server contract. Today
// collectAll stops SILENTLY on an unchanged cursor (paging.go:15), so this
// read succeeds with a truncated list; ruling T14-15 (review r1 M-1) says it
// must error instead.
func TestRegistryRepositoriesDataSource_RepeatedCursorErrors(t *testing.T) {
	api := setupFake(t)
	api.SeedRepository(testOrg, "app", 1, 10, nil)
	api.SetRepeatingHarborCursor(testOrg, "2")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      repositoriesDSHCL,
			ExpectError: loose("repeated"),
		}},
	})
}

// RP3. A registry disabled without purge still serves its retained images -
// no error, the list is exactly what was seeded before the disable.
func TestRegistryRepositoriesDataSource_DisabledWithoutPurgeStillLists(t *testing.T) {
	api := setupFake(t)
	api.SeedRepository(testOrg, "keep", 2, 512, nil)
	rawEnableRegistry(t, api, testOrg)
	api.Disable(testOrg) // registryStore only; repositoryStore is untouched
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: repositoriesDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.#", "1"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.0.name", "keep"),
			),
		}},
	})
}

// Coder's suggested pin 2 (task-14-report.md "Fix round 2", review r1 N-5):
// a page whose body OMITS the "next_cursor" key entirely (not merely sends
// it as "") must still be read as the last page - a future API move to the
// common ListResponse (aether-api#311, omitempty) would otherwise break
// every repositories/tags read. Exactly one GET, no error, items present.
func TestRegistryRepositoriesDataSource_AbsentNextCursorKeyEndsWalk(t *testing.T) {
	api := setupFake(t)
	api.SeedRepository(testOrg, "solo", 1, 100, nil)
	api.SetOmitNextCursor(testOrg, true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: repositoriesDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.#", "1"),
				resource.TestCheckResourceAttr(repositoriesDataSource, "repositories.0.name", "solo"),
				func(*terraform.State) error {
					gets := requestsMatching(api, http.MethodGet, `/registry/repositories$`)
					if len(gets) != 1 {
						return fmt.Errorf("GET repositories calls = %d, want exactly 1 (an absent next_cursor key must be treated as the last page)", len(gets))
					}
					return nil
				},
			),
		}},
	})
}
