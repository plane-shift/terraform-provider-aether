package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
)

var (
	_ resource.Resource                = &organizationMemberResource{}
	_ resource.ResourceWithConfigure   = &organizationMemberResource{}
	_ resource.ResourceWithImportState = &organizationMemberResource{}
)

// memberListPageSize is the page size of the last-owner guard's member list
// (the API's maximum).
const memberListPageSize = 100

// memberListMaxPages bounds the guard's cursor loop (10,000 members), so an
// API that repeated a cursor could not spin it until the context expired.
const memberListMaxPages = 100

// lastOwnerRefusal is the T9-4 diagnostic, verbatim.
const lastOwnerRefusal = "refusing to remove the last owner: the API has no guard (aether-api #302) and the organization would become unmanageable (roles and deletion need an owner login)"

// NewOrganizationMemberResource returns the aether_organization_member resource.
func NewOrganizationMemberResource() resource.Resource { return &organizationMemberResource{} }

// organizationMemberResource manages one membership of the provider's
// organization. There is no in-place update: the API refuses every member PUT
// made with an API token (403 token_not_permitted), so both attributes force a
// replacement (DELETE, then POST).
type organizationMemberResource struct{ pd *providerData }

// organizationMemberModel is the aether_organization_member state.
type organizationMemberModel struct {
	ID         types.String `tfsdk:"id"`
	Email      types.String `tfsdk:"email"`
	Role       types.String `tfsdk:"role"`
	InvitedBy  types.String `tfsdk:"invited_by"`
	AcceptedAt types.String `tfsdk:"accepted_at"`
	CreatedAt  types.String `tfsdk:"created_at"`
}

// Metadata names the resource aether_organization_member.
func (r *organizationMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization_member"
}

// Schema defines aether_organization_member.
func (r *organizationMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A member of the provider's organization. " +
			"The API creates the membership only. The address must already have an Aether login account (created through the portal invitation flow; aether-api #302), otherwise the member cannot sign in. Destroying the resource removes the membership and leaves the login account in place. " +
			"There is no in-place update: changing `email` or `role` replaces the membership (it is removed, then created again with a new `id`). Do not set `create_before_destroy`: the new membership would be created while the old one still holds the address, which the API refuses (409). " +
			"Removing or replacing the organization's last `owner` is refused, because the API has no such guard and nobody could change roles or delete the organization afterwards. The check reads the member's current role from the API and is serialised within a single run; the platform itself has no guard, so a concurrent removal in the portal or another run is not prevented. " +
			"Transferring ownership in one apply (adding the new owner and demoting or removing the old one) depends on the order Terraform runs the two changes, and is refused when the old owner goes first: give the demoted owner's resource a `depends_on` on the new owner's resource, or use two applies (add the new owner, then demote the old one).",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("Membership UUID (not a user id). It changes when the membership is replaced."),
			"email": schema.StringAttribute{Required: true,
				MarkdownDescription: "Login email address of the member. It must be lower-case: membership matches the login email exactly. Changing it replaces the membership.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{memberEmailValidator{}}},
			"role": schema.StringAttribute{Required: true,
				MarkdownDescription: "One of `owner`, `admin`, `member`, `viewer`. Changing it replaces the membership: a DELETE followed by a POST, which gives the membership a new `id`. " +
					"The portal never offers `owner`. An `owner` can be demoted from Terraform like any other role; only the last owner is protected, and removing or replacing it is refused.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.OneOf("owner", "admin", "member", "viewer")}},
			"invited_by":  immutableComputed("UUID of the user who invited the member. Always null for a membership created through the API."),
			"accepted_at": immutableComputed("Time the invitation was accepted (RFC 3339). Always null for a membership created through the API."),
			"created_at":  immutableComputed("Creation time of the membership (RFC 3339)."),
		},
	}
}

// memberEmailValidator refuses upper-case characters (the API stores the
// address verbatim and matches the login email exactly, case-sensitively)
// and requires exactly one `@`. No normalisation is done.
type memberEmailValidator struct{}

func (memberEmailValidator) Description(context.Context) string {
	return "must be lower-case and contain exactly one @"
}

func (v memberEmailValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (memberEmailValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	if strings.ToLower(s) != s {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid email",
			fmt.Sprintf("%q must be lower-case: membership matches the login email exactly.", s))
	}
	if strings.Count(s, "@") != 1 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid email",
			fmt.Sprintf("%q must contain exactly one @.", s))
	}
}

// Configure receives the provider's client and organization.
func (r *organizationMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// Create adds the membership with one POST of exactly {email, role}.
func (r *organizationMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan organizationMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := client.CreateMemberRequest{
		Email: plan.Email.ValueString(),
		Role:  client.CreateMemberRequestRole(plan.Role.ValueString()),
	}
	res, err := r.pd.Client.CreateMemberWithResponse(ctx, r.pd.orgID, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot add organization member", err.Error())
		return
	}
	if res.StatusCode() != http.StatusCreated {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	if res.JSON201 == nil {
		resp.Diagnostics.AddError("Aether API accepted the member but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+". The membership was probably created, but it is not recorded in state: import it by its id, or remove it in the portal and apply again.")
		return
	}
	flattenMember(res.JSON201, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the membership. 404 (removed, or not this organization's)
// removes it from state; every other failure, a 403 included, is an error and
// keeps state.
func (r *organizationMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state organizationMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid id in state", err.Error())
		return
	}
	g, err := r.pd.Client.GetMemberWithResponse(ctx, r.pd.orgID, id)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read organization member", err.Error())
		return
	}
	switch {
	case g.StatusCode() == http.StatusNotFound:
		resp.State.RemoveResource(ctx)
		return
	case g.StatusCode() != http.StatusOK:
		addAPIError(&resp.Diagnostics, g.StatusCode(), g.HTTPResponse.Header, g.Body, path.Empty())
		return
	case g.JSON200 == nil:
		resp.Diagnostics.AddError("Aether API returned no usable organization member", describeNonJSON(g.HTTPResponse, g.Status()))
		return
	}
	flattenMember(g.JSON200, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never reached with a change to make: every configurable attribute
// forces a replacement, and a member PUT is always 403 for an API token. It
// therefore sends nothing and keeps the planned (= prior) values.
func (r *organizationMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan organizationMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the membership. 204 and 404 (already gone) are success.
//
// The last-owner guard (T9-4, amended r1 = T9-14) decides on the LIVE role,
// never the role in state: the member is re-read first (GET), so a membership
// promoted to owner out of band is still guarded even under -refresh=false. A
// 404 on that read means the membership is already gone: success, no DELETE.
//
// For an owner, the guard's member list AND the DELETE run under
// providerData.ownerDeleteMu. Terraform deletes resources in parallel, and
// without the lock two owner deletes in one run (a destroy of a config that
// manages both of the org's owners) would each see the other as "another
// owner" and both succeed, leaving the organization with none. The lock is
// per provider process, i.e. per Terraform run: it cannot serialise against
// the portal or a second run - the platform itself has no guard (aether-api
// #302). Non-owner deletes take neither the lock nor the list.
func (r *organizationMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state organizationMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid id in state", err.Error())
		return
	}
	g, err := r.pd.Client.GetMemberWithResponse(ctx, r.pd.orgID, id)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read organization member for the last-owner check", err.Error())
		return
	}
	switch {
	case g.StatusCode() == http.StatusNotFound:
		return // already gone
	case g.StatusCode() != http.StatusOK:
		addAPIError(&resp.Diagnostics, g.StatusCode(), g.HTTPResponse.Header, g.Body, path.Empty())
		return
	case g.JSON200 == nil:
		resp.Diagnostics.AddError("Aether API returned no usable organization member", describeNonJSON(g.HTTPResponse, g.Status()))
		return
	}
	if g.JSON200.Role == client.MemberRoleOwner {
		r.pd.ownerDeleteMu.Lock()
		defer r.pd.ownerDeleteMu.Unlock()
		gone, d := r.guardLastOwner(ctx, id)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() || gone {
			return
		}
	}
	res, err := r.pd.Client.DeleteMemberWithResponse(ctx, r.pd.orgID, id)
	if err != nil {
		resp.Diagnostics.AddError("Cannot remove organization member", err.Error())
		return
	}
	if code := res.StatusCode(); code != http.StatusNoContent && code != http.StatusNotFound {
		addAPIError(&resp.Diagnostics, code, res.HTTPResponse.Header, res.Body, path.Empty())
	}
}

// guardLastOwner lists every member (following next_cursor; a repeated cursor
// or more than memberListMaxPages pages is an error) and refuses with an error when no owner OTHER
// than id exists: the API has no such guard, and an organization without an
// owner can no longer change roles or be deleted. gone reports that the
// membership is already gone - a 404 during the check, or id absent from the
// complete list - so there is nothing to delete and no owner to lose. The
// caller holds providerData.ownerDeleteMu.
func (r *organizationMemberResource) guardLastOwner(ctx context.Context, id client.MemberID) (gone bool, diags diag.Diagnostics) {
	limit := memberListPageSize
	params := &client.ListMembersParams{Limit: &limit}
	seen := false
	cursors := map[string]bool{} // every cursor followed so far
	for page := 1; ; page++ {
		if page > memberListMaxPages {
			diags.AddError("Cannot complete the last-owner check",
				fmt.Sprintf("The member list did not end within %d pages of %d members; the API may be repeating a cursor. The membership was not removed.", memberListMaxPages, memberListPageSize))
			return false, diags
		}
		res, err := r.pd.Client.ListMembersWithResponse(ctx, r.pd.orgID, params)
		if err != nil {
			diags.AddError("Cannot list organization members for the last-owner check", err.Error())
			return false, diags
		}
		switch {
		case res.StatusCode() == http.StatusNotFound:
			return true, diags
		case res.StatusCode() != http.StatusOK:
			addAPIError(&diags, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
			return false, diags
		case res.JSON200 == nil:
			diags.AddError("Aether API returned no usable member list", describeNonJSON(res.HTTPResponse, res.Status()))
			return false, diags
		}
		for _, m := range res.JSON200.Items {
			if m.Id == id {
				seen = true
			} else if m.Role == client.MemberRoleOwner {
				return false, diags
			}
		}
		next := res.JSON200.NextCursor
		if next == nil || *next == "" {
			break
		}
		cursor, err := parseID(*next)
		if err != nil {
			diags.AddError("Aether API returned an unusable member list cursor", err.Error())
			return false, diags
		}
		// A cursor already followed would return the same page again: fail
		// now rather than loop until the page bound.
		if cursors[cursor.String()] {
			diags.AddError("Cannot complete the last-owner check",
				fmt.Sprintf("The member list returned cursor %s a second time, so the list would never end. The membership was not removed.", cursor))
			return false, diags
		}
		cursors[cursor.String()] = true
		params = &client.ListMembersParams{Limit: &limit, Cursor: &cursor}
	}
	if !seen {
		return true, diags
	}
	diags.AddAttributeError(path.Root("role"), "Cannot remove the last owner", lastOwnerRefusal+
		". Add another owner first (in the portal, or with another aether_organization_member), or remove this membership in the portal.")
	return false, diags
}

// ImportState imports a membership by its membership id (canonical UUID
// text; not a user id); Read fills in the rest.
func (r *organizationMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := parseID(req.ID)
	if err != nil || id.String() != req.ID {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Expected the membership's id as a canonical lower-case UUID (36 characters), got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id.String())...)
}

// flattenMember copies an API member into the model. invited_by and
// accepted_at are absent on API-created memberships and stay null.
func flattenMember(m *client.Member, s *organizationMemberModel) {
	s.ID = types.StringValue(m.Id.String())
	s.Email = types.StringValue(m.Email)
	s.Role = types.StringValue(string(m.Role))
	s.InvitedBy = types.StringNull()
	if m.InvitedBy != nil {
		s.InvitedBy = types.StringValue(m.InvitedBy.String())
	}
	s.AcceptedAt = types.StringNull()
	if m.AcceptedAt != nil {
		s.AcceptedAt = types.StringValue(m.AcceptedAt.Format(time.RFC3339))
	}
	s.CreatedAt = types.StringValue(m.CreatedAt.Format(time.RFC3339))
}
