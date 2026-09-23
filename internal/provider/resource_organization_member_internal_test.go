package provider

// Pins the user-facing descriptions of aether_organization_member that
// black-box tests cannot reach (plugin-testing v1.16.0 exposes no schema).
// Named Test_... so `-run organizationMember` matches (Go only treats a name
// as a test if the character after "Test" is not a lowercase letter).

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
)

func memberSchemaDescriptions(t *testing.T) (resourceDesc, roleDesc string) {
	t.Helper()
	var resp resource.SchemaResponse
	NewOrganizationMemberResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	return resp.Schema.MarkdownDescription, resp.Schema.Attributes["role"].GetMarkdownDescription()
}

// Fails if the owner-ruled T9-10 sentence is edited, reworded or dropped from
// the resource description (it is the only place a practitioner learns a
// Terraform-created member may be unable to sign in).
func Test_organizationMember_DescriptionCarriesTheT910SentenceVerbatim(t *testing.T) {
	desc, _ := memberSchemaDescriptions(t)
	const want = "The API creates the membership only. The address must already have an Aether login account " +
		"(created through the portal invitation flow; aether-api #302), otherwise the member cannot sign in. " +
		"Destroying the resource removes the membership and leaves the login account in place."
	if !strings.Contains(desc, want) {
		t.Fatalf("resource description lacks the T9-10 sentence verbatim:\n%s", desc)
	}
}

// Fails if the role description keeps the retired claim that granting owner
// "cannot be undone" (false since T9-1: a demotion is DELETE + POST, which
// tokens may do), or stops saying that only the LAST owner is protected.
func Test_organizationMember_RoleDescriptionStatesOnlyTheLastOwnerIsProtected(t *testing.T) {
	_, role := memberSchemaDescriptions(t)
	if strings.Contains(role, "cannot be undone") {
		t.Fatalf("role description still claims owner cannot be undone: %s", role)
	}
	if !strings.Contains(role, "last owner") {
		t.Fatalf("role description does not mention the last owner: %s", role)
	}
}
