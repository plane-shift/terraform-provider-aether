package provider_test

// Tests for the aether_registry_repository_tags data source
// (datasource_registry_repository_tags.go, T14-5, registered as
// NewRegistryRepositoryTagsDataSource in provider.go's DataSources()):
// raw-decoded vulnerability counts, nested repository path escaping, and the
// collectAll repeated-cursor guard below (shares the Harbor-page pager with
// datasource_registry_repositories_test.go).

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

func tagsHCL(repo string) string {
	return fmt.Sprintf(`
provider "aether" {}
data "aether_registry_repository_tags" "t" {
  repository = %q
}
`, repo)
}

const tagsDataSource = "data.aether_registry_repository_tags.t"

// TG1. Fails if any tag attribute is wrong: raw-decoded vulnerabilities
// (including low/unknown, which CG's VulnSummary drops), a null
// vulnerabilities object when the artifact was never scanned (distinct from
// an all-zero one), null pushed_at/pulled_at when absent, an untagged item's
// empty name, or the final ORDER (digest, then name - T14-5).
//
// The scanned tag at digest "ccc" carries five DISTINCT non-zero counts
// (review r1 I-1: an all-zero seed cannot catch low/unknown being dropped or
// hard-coded to 0, nor a field swap such as low<-medium - each of the five
// is asserted individually against its own value).
func TestRegistryRepositoryTagsDataSource_ReadAllAttributesSorted(t *testing.T) {
	api := setupFake(t)
	pushed := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	pulled := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	// digest "aaa" untagged (name "", not scanned) sorts before digest "bbb"
	// v2 (scanned, zero findings) which sorts before digest "bbb" v1, which
	// sorts before digest "ccc" (scanned, distinct non-zero findings) -
	// proves the sort is (digest, name), not insertion order or name alone.
	api.SeedTag(testOrg, "app", "", "sha256:aaa", 10, nil, nil, true, nil)
	api.SeedTag(testOrg, "app", "v1", "sha256:bbb", 20, &pushed, &pulled, false, &fakeapi.Vulnerabilities{})
	api.SeedTag(testOrg, "app", "a-earlier", "sha256:bbb", 20, &pushed, &pulled, false, nil)
	api.SeedTag(testOrg, "app", "v2", "sha256:ccc", 30, &pushed, &pulled, false,
		&fakeapi.Vulnerabilities{Critical: 1, High: 2, Medium: 3, Low: 4, Unknown: 5})

	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: tagsHCL("app"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(tagsDataSource, "tags.#", "4"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.0.digest", "sha256:aaa"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.0.name", ""),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.0.untagged", "true"),
				resource.TestCheckNoResourceAttr(tagsDataSource, "tags.0.pushed_at"),
				resource.TestCheckNoResourceAttr(tagsDataSource, "tags.0.pulled_at"),
				resource.TestCheckNoResourceAttr(tagsDataSource, "tags.0.vulnerabilities"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.1.digest", "sha256:bbb"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.1.name", "a-earlier"),
				resource.TestCheckNoResourceAttr(tagsDataSource, "tags.1.vulnerabilities"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.digest", "sha256:bbb"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.name", "v1"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.pushed_at", pushed.Format(time.RFC3339)),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.pulled_at", pulled.Format(time.RFC3339)),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.vulnerabilities.critical", "0"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.vulnerabilities.high", "0"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.vulnerabilities.medium", "0"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.vulnerabilities.low", "0"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.2.vulnerabilities.unknown", "0"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.3.digest", "sha256:ccc"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.3.name", "v2"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.3.vulnerabilities.critical", "1"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.3.vulnerabilities.high", "2"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.3.vulnerabilities.medium", "3"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.3.vulnerabilities.low", "4"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.3.vulnerabilities.unknown", "5"),
			),
		}},
	})
}

// TG2. A nested repository name ("team/app") must reach the fake's wire as
// ONE escape (`team%2Fapp`), never a literal unescaped "/" (which would
// mis-route) and never a double escape (`team%252Fapp`, Harbor's own
// accidental convention - contract §2 U4 says that quirk belongs between
// aether-api and Harbor, not this client).
func TestRegistryRepositoryTagsDataSource_NestedNameEscapedOnce(t *testing.T) {
	api := setupFake(t)
	api.SeedTag(testOrg, "team/app", "v1", "sha256:ccc", 5, nil, nil, false, nil)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: tagsHCL("team/app"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(tagsDataSource, "tags.#", "1"),
				resource.TestCheckResourceAttr(tagsDataSource, "tags.0.digest", "sha256:ccc"),
				func(*terraform.State) error {
					for _, r := range api.Requests() {
						if r.Method != http.MethodGet {
							continue
						}
						if r.RawPath == "" {
							continue
						}
						if wantEscapedNestedSegment(r.RawPath) {
							return nil
						}
					}
					var raw []string
					for _, r := range api.Requests() {
						raw = append(raw, r.RawPath)
					}
					return fmt.Errorf("no GET request's RawPath carried exactly \"team%%2Fapp\" (single-escaped); saw: %v", raw)
				},
			),
		}},
	})
}

// wantEscapedNestedSegment reports whether raw contains the single-escaped
// segment "team%2Fapp" and NOT the double-escaped "team%252Fapp".
func wantEscapedNestedSegment(raw string) bool {
	return strings.Contains(raw, "team%2Fapp") && !strings.Contains(raw, "team%252Fapp")
}

// T14-15 (fixed by Task 13's collectAll, carried forward at rebase - see
// task-14-tests-report.md "Fix round 1"): the tags route shares the same
// Harbor-page collectAll pager as repositories (TestRegistryRepositories
// DataSource_RepeatedCursorErrors); collectAll tracks every cursor it has
// already sent and errors here too the moment the API repeats one, not a
// silently truncated list (ruling T14-15, review r1 M-1).
func TestRegistryRepositoryTagsDataSource_RepeatedCursorErrors(t *testing.T) {
	api := setupFake(t)
	api.SeedTag(testOrg, "app", "v1", "sha256:aaa", 10, nil, nil, false, nil)
	api.SetRepeatingHarborCursor(testOrg, "2")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      tagsHCL("app"),
			ExpectError: loose("repeated"),
		}},
	})
}

// TG3. A missing repository (or a never-enabled registry, indistinguishable
// on the wire) is a 404 the data source must surface as a plan-time error.
func TestRegistryRepositoryTagsDataSource_MissingRepoErrors(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      tagsHCL("nope"),
			ExpectError: loose("repository not found"),
		}},
	})
}
