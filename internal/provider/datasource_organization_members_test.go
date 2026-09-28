package provider_test

// Tests for the aether_organization_members data source (not yet
// implemented - datasource_organization_members.go, T14-1 of
// task-14-brief-amended.md). Until NewOrganizationMembersDataSource is
// registered in provider.go's DataSources(), EVERY test in this file fails
// at plan time with terraform-plugin-framework's own "Invalid Data Source
// Type" diagnostic (mirrors task-13-brief.md's note on resources) - the
// expected RED state. A wrong (but present) implementation fails on the
// specific assertion instead, which is how each test below is shown able to
// fail on more than just "missing".

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const membersDataSource = "data.aether_organization_members.m"

const membersDSHCL = `
provider "aether" {}
data "aether_organization_members" "m" {}
`

// Fails if any of id/email/role/created_at is missing from the flattened
// member, or if invited_by/accepted_at (never written by the fake's
// API-created rows - contract §10 "no code path writes them") are anything
// but null.
func TestOrganizationMembersDataSource_ReadAllAttributes(t *testing.T) {
	api := setupFake(t)
	id := api.SeedMember(testOrg, "member@example.com", "viewer")
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: membersDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(membersDataSource, "members.#", "1"),
				resource.TestCheckResourceAttr(membersDataSource, "members.0.id", id),
				resource.TestCheckResourceAttr(membersDataSource, "members.0.email", "member@example.com"),
				resource.TestCheckResourceAttr(membersDataSource, "members.0.role", "viewer"),
				resource.TestCheckResourceAttrSet(membersDataSource, "members.0.created_at"),
				resource.TestCheckNoResourceAttr(membersDataSource, "members.0.invited_by"),
				resource.TestCheckNoResourceAttr(membersDataSource, "members.0.accepted_at"),
			),
		}},
	})
}

// T14-15 (RED until Task 13's collectAll fix lands here at rebase - see
// task-14-tests-report.md "Fix round 1"): the members data source is a
// collectAll consumer distinct from the Harbor-page pager (compare
// TestRegistryRepositoriesDataSource_RepeatedCursorErrors). Today collectAll
// stops SILENTLY on an unchanged cursor (paging.go:15), so this read
// succeeds with a truncated list; ruling T14-15 (review r1 M-1) says it must
// error instead.
func TestOrganizationMembersDataSource_RepeatedCursorErrors(t *testing.T) {
	api := setupFake(t)
	const cursor = "11111111-1111-4111-8111-111111111111"
	api.SeedMemberWithID(testOrg, cursor, "member@example.com", "viewer")
	api.SetRepeatingMemberCursor(testOrg, cursor)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      membersDSHCL,
			ExpectError: loose("repeated"),
		}},
	})
}

// MB1. Fails if the data source does not walk every page of a >100-member
// organization (collectAll with limit=100, T14-1): asserts exactly 2 GET
// requests were made, the first with limit=100 and no cursor, and that all
// 150 seeded members land in state.
func TestOrganizationMembersDataSource_WalksTwoPages(t *testing.T) {
	api := setupFake(t)
	for i := 0; i < 150; i++ {
		api.SeedMember(testOrg, fmt.Sprintf("m%03d@example.com", i), "viewer")
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: membersDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(membersDataSource, "members.#", "150"),
				func(*terraform.State) error {
					gets := requestsMatching(api, http.MethodGet, `/members$`)
					if len(gets) != 2 {
						return fmt.Errorf("GET /members calls = %d, want exactly 2 (150 members, limit=100 per T14-1)", len(gets))
					}
					if gets[0].Query != "limit=100" {
						return fmt.Errorf("first GET query = %q, want exactly \"limit=100\" (no cursor on page 1)", gets[0].Query)
					}
					return nil
				},
			),
		}},
	})
}
