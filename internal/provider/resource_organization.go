package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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
	_ resource.Resource                = &organizationResource{}
	_ resource.ResourceWithConfigure   = &organizationResource{}
	_ resource.ResourceWithImportState = &organizationResource{}
	_ resource.ResourceWithModifyPlan  = &organizationResource{}
)

// NewOrganizationResource returns the aether_organization resource.
func NewOrganizationResource() resource.Resource { return &organizationResource{} }

// organizationResource manages the settings of the provider's own
// organization. It is IMPORT-ONLY: organizations are created at registration
// (Create is refused), and an API token can never delete one, so destroy only
// forgets it. Every operation is at most one request; nothing is waited for.
type organizationResource struct{ pd *providerData }

// organizationModel is the aether_organization state.
type organizationModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Slug         types.String `tfsdk:"slug"`
	BillingEmail types.String `tfsdk:"billing_email"`
	VatID        types.String `tfsdk:"vat_id"`
	Plan         types.String `tfsdk:"plan"`
	CreatedAt    types.String `tfsdk:"created_at"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
}

// slugPattern is aether-api's own slug rule (the public check-slug endpoint).
// The API does NOT apply it on PUT, so the provider does.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// Metadata names the resource aether_organization.
func (r *organizationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization"
}

// managedString is an Optional+Computed setting (T9-5): unset in config means
// "not managed" and keeps the stored value (nothing is sent); `""` clears it.
// The API is a COALESCE merge, so a stored value can never go back to null.
func managedString(desc string, validators ...validator.String) schema.StringAttribute {
	return schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		Validators:    validators}
}

// Schema defines aether_organization.
func (r *organizationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The settings of the provider's organization. Organizations are created at registration, so this resource cannot create one: import it (`terraform import aether_organization.<name> <organization id>`). " +
			"Destroy only forgets the organization; it is never deleted. " +
			"Removing `name`, `billing_email` or `vat_id` from the configuration stops managing it and keeps the stored value; set it to `\"\"` to clear it (`name` cannot be cleared).",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("Organization UUID."),
			"name": managedString("Display name of the organization. Must not be empty.",
				stringvalidator.LengthAtLeast(1)),
			"slug": managedString("URL-safe organization identifier (3-63 characters: lower-case letters, digits and hyphens, not starting or ending with a hyphen). "+
				"It cannot be changed through Terraform: registry projects, storage billing and quota are keyed on it, and a rename would strand them for existing clusters (aether-api #303). Rename in the portal instead.",
				stringvalidator.RegexMatches(slugPattern, "must be 3-63 characters of lower-case letters, digits and hyphens, not starting or ending with a hyphen")),
			"billing_email": managedString("Address invoices are sent to. `\"\"` clears it.", billingEmailValidator{}),
			"vat_id":        managedString("VAT identification number. `\"\"` clears it."),
			"plan":          immutableComputed("Legacy organization plan, always `starter`; not settable."),
			"created_at":    immutableComputed("Creation time (RFC 3339)."),
			// No UseStateForUnknown: the API moves it on EVERY PUT.
			"updated_at": observedComputed("Time the organization was last written (RFC 3339)."),
		},
	}
}

// billingEmailValidator requires exactly one `@` in a non-empty value (`""`
// is the clear signal and passes). The API itself checks nothing.
type billingEmailValidator struct{}

func (billingEmailValidator) Description(context.Context) string {
	return "must contain exactly one @ (or be empty, to clear it)"
}

func (v billingEmailValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (billingEmailValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if s := req.ConfigValue.ValueString(); s != "" && strings.Count(s, "@") != 1 {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid billing_email",
			fmt.Sprintf("%q must contain exactly one @ (or be \"\" to clear the billing email).", s))
	}
}

// Configure receives the provider's client and organization.
func (r *organizationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan refuses a slug change (T9-6). The API accepts one with 200 but
// existing Harbor projects, storage metering and quota keep the old slug, so
// they silently split from the organization (aether-api #303). Only a set,
// known configuration value that DIFFERS from the known stored slug is a
// rename; restating the current slug is fine.
func (r *organizationResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var stateSlug, configSlug types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("slug"), &stateSlug)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("slug"), &configSlug)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if stateSlug.IsNull() || stateSlug.IsUnknown() || configSlug.IsNull() || configSlug.IsUnknown() {
		return
	}
	if configSlug.ValueString() != stateSlug.ValueString() {
		resp.Diagnostics.AddAttributeError(path.Root("slug"), "The organization slug cannot be changed",
			fmt.Sprintf("Changing the slug from %q to %q is refused: registry images, storage billing and quota of existing clusters stay keyed on the old slug and would be stranded (aether-api #303). "+
				"Rename the organization in the portal, or restore slug = %q.", stateSlug.ValueString(), configSlug.ValueString(), stateSlug.ValueString()))
	}
}

// Create is REFUSED (T9-8): organizations are created at registration. It
// makes no request.
func (r *organizationResource) Create(_ context.Context, _ resource.CreateRequest, resp *resource.CreateResponse) {
	resp.Diagnostics.AddError("Organizations are created at registration",
		"Import your organization instead: terraform import aether_organization.<name> <organization id> (data.aether_organization exposes the id).")
}

// Read refreshes the organization. 404 removes it from state; every other
// failure, a 403 (the organization is not this token's) included, is an error
// and keeps state.
func (r *organizationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state organizationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid id in state", err.Error())
		return
	}
	g, err := r.pd.Client.GetOrganizationWithResponse(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read organization", err.Error())
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
		resp.Diagnostics.AddError("Aether API returned no usable organization", describeNonJSON(g.HTTPResponse, g.Status()))
		return
	}
	flattenOrganization(g.JSON200, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update sends ONLY the attributes whose planned value is known, non-null and
// different from state (T9-7). The PUT is a COALESCE merge, so an omitted key
// keeps the stored value; `""` is sent as-is and clears. `{}` is never sent -
// it would still move updated_at - so a plan with nothing to write reads
// instead.
func (r *organizationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state organizationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid id in state", err.Error())
		return
	}
	var body client.UpdateOrganizationRequest
	body.Name = changedString(plan.Name, state.Name)
	body.Slug = changedString(plan.Slug, state.Slug)
	body.BillingEmail = changedString(plan.BillingEmail, state.BillingEmail)
	body.VatId = changedString(plan.VatID, state.VatID)

	if body == (client.UpdateOrganizationRequest{}) {
		g, err := r.pd.Client.GetOrganizationWithResponse(ctx, id)
		if err != nil {
			resp.Diagnostics.AddError("Cannot read organization", err.Error())
			return
		}
		if g.StatusCode() != http.StatusOK {
			addAPIError(&resp.Diagnostics, g.StatusCode(), g.HTTPResponse.Header, g.Body, path.Empty())
			return
		}
		if g.JSON200 == nil {
			resp.Diagnostics.AddError("Aether API returned no usable organization", describeNonJSON(g.HTTPResponse, g.Status()))
			return
		}
		flattenOrganization(g.JSON200, &plan)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	res, err := r.pd.Client.UpdateOrganizationWithResponse(ctx, id, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update organization", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	if res.JSON200 == nil {
		resp.Diagnostics.AddError("Aether API accepted the organization update but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+". The change was probably written, but state still holds the previous values: apply again (the PUT is idempotent).")
		return
	}
	flattenOrganization(res.JSON200, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete is STATE-ONLY (T9-8): it makes no request and the framework removes
// the resource from state. The organization is never deleted (an API token
// could not anyway: DELETE is 403 token_not_permitted).
func (r *organizationResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}

// ImportState imports the organization by its UUID; Read fills in the rest.
func (r *organizationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := parseID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import id", "Expected the organization's UUID: "+err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id.String())...)
}

// changedString returns the planned value to send, or nil when it must not be
// sent: unknown, null (never sent - the API would keep the value anyway, and
// T9-5 makes null mean "not managed"), or equal to state.
func changedString(plan, state types.String) *string {
	if plan.IsNull() || plan.IsUnknown() || plan.Equal(state) {
		return nil
	}
	s := plan.ValueString()
	return &s
}

// flattenOrganization copies an API organization into the model. An absent
// billing_email/vat_id (stored NULL) is null; a present `""` stays `""`.
func flattenOrganization(o *client.Organization, m *organizationModel) {
	m.ID = types.StringValue(o.Id.String())
	m.Name = types.StringValue(o.Name)
	m.Slug = types.StringValue(o.Slug)
	m.BillingEmail = optString(o.BillingEmail)
	m.VatID = optString(o.VatId)
	m.Plan = optString(o.Plan)
	m.CreatedAt = types.StringValue(o.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(o.UpdatedAt.Format(time.RFC3339))
}
