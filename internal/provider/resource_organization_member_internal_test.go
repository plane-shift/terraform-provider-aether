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

// Fails if the resource description stops saying that member emails are
// compared case-insensitively (a case variant of an existing member is
// refused), or reverts to the retired claim that membership matches the login
// email exactly. The email attribute must say the same.
func Test_organizationMember_DescriptionsStateCaseInsensitiveEmails(t *testing.T) {
	desc, _ := memberSchemaDescriptions(t)
	const want = "Member emails are compared case-insensitively: adding an address that differs from an existing member's only in letter case is refused (409)."
	if !strings.Contains(desc, want) {
		t.Fatalf("resource description lacks the case-insensitive sentence verbatim:\n%s", desc)
	}
	var resp resource.SchemaResponse
	NewOrganizationMemberResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	email := resp.Schema.Attributes["email"].GetMarkdownDescription()
	if !strings.Contains(email, "The API compares member emails case-insensitively") {
		t.Fatalf("email description does not state the case-insensitive comparison: %s", email)
	}
	for name, d := range map[string]string{"resource": desc, "email": email} {
		if strings.Contains(d, "matches the login email exactly") {
			t.Fatalf("%s description keeps the retired exact-match claim: %s", name, d)
		}
	}
}

// Fails if the resource description drops the instruction to revoke a removed
// owner's API tokens (a token outlives the membership of whoever created it),
// or the portal path where that is done.
func Test_organizationMember_DescriptionTellsToRevokeARemovedOwnersTokens(t *testing.T) {
	desc, _ := memberSchemaDescriptions(t)
	const want = "An API token is not tied to the member who created it: after removing an owner " +
		"(or replacing their membership with a lower role), revoke the API tokens they created in the portal (**Settings → API Tokens**)."
	if !strings.Contains(desc, want) {
		t.Fatalf("resource description lacks the revoke-tokens sentence verbatim:\n%s", desc)
	}
}

// Fails if the resource description keeps the retired claim that the
// platform has no last-owner guard, or stops naming the API's refusal.
func Test_organizationMember_DescriptionNamesTheAPILastOwnerRefusal(t *testing.T) {
	desc, _ := memberSchemaDescriptions(t)
	if strings.Contains(desc, "has no guard") || strings.Contains(desc, "has no such guard") {
		t.Fatalf("resource description keeps the retired no-guard claim:\n%s", desc)
	}
	if !strings.Contains(desc, "the API also refuses to remove the last owner (409 `last_owner`)") {
		t.Fatalf("resource description does not name the API's 409 last_owner refusal:\n%s", desc)
	}
}
