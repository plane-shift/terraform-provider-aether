package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var (
	_ resource.Resource                = &registryResource{}
	_ resource.ResourceWithConfigure   = &registryResource{}
	_ resource.ResourceWithImportState = &registryResource{}
)

// NewRegistryResource returns the aether_registry resource.
func NewRegistryResource() resource.Resource { return &registryResource{} }

// registryResource manages the ONE container registry of the provider's
// organization (a singleton: the API addresses it by organization, never by
// id). Create is a bodyless POST (plus a GET on the 409 adopt path), Read a
// GET, Delete a DELETE; Update touches nothing but state, because the only
// updatable attribute (purge_on_destroy) exists only in Terraform. No
// timeouts (T11-15).
type registryResource struct{ pd *providerData }

// registryModel is the aether_registry state; tfsdk tags are the attribute names.
type registryModel struct {
	ID             types.String `tfsdk:"id"`
	HarborProject  types.String `tfsdk:"harbor_project"`
	Endpoint       types.String `tfsdk:"endpoint"`
	Enabled        types.Bool   `tfsdk:"enabled"`
	QuotaGB        types.Int64  `tfsdk:"quota_gb"`
	RobotUsername  types.String `tfsdk:"robot_username"`
	RobotSecret    types.String `tfsdk:"robot_secret"`
	PurgeOnDestroy types.Bool   `tfsdk:"purge_on_destroy"`
	CreatedAt      types.String `tfsdk:"created_at"`
	UpdatedAt      types.String `tfsdk:"updated_at"`
}

// Metadata names the resource aether_registry.
func (r *registryResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry"
}

// Schema defines aether_registry. Every API-derived attribute is pinned with
// UseStateForUnknown: no route updates a registry row (contract §2), so a
// value once read never changes while the resource exists.
func (r *registryResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The container registry of the provider's organization (one per organization). Creating this resource enables the registry and mints its default robot credential; " +
			"`robot_username` and `robot_secret` are returned by the API only by that enable call. Credentials rotate in the portal, not through this resource. " +
			"Destroying this resource disables the registry. Without `purge_on_destroy` the images remain in the platform's project and are restored by a later enable; with it EVERY repository is deleted first (irreversible). " +
			"Disabling also deletes EVERY robot account of the organization, including `aether_registry_robot_account` resources (they are recreated on the next apply) — order destroys with `depends_on` from the robot accounts to the registry. " +
			"Cleanup policies are not affected.",
		Attributes: map[string]schema.Attribute{
			"id":             immutableComputed("Registry UUID."),
			"harbor_project": immutableComputed("The organization's registry project (`aether-<organization slug>`)."),
			"endpoint":       immutableComputed("Push/pull prefix: `<registry host>/<harbor_project>`."),
			"enabled": schema.BoolAttribute{Computed: true,
				MarkdownDescription: "Always `true` while the resource exists (a disabled registry is removed from state).",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}},
			"quota_gb": schema.Int64Attribute{Computed: true,
				MarkdownDescription: "Storage quota in GB; informational; not enforced by the platform today.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"robot_username": schema.StringAttribute{Computed: true,
				MarkdownDescription: "The default robot's `docker login` username. Known only from the enable call; import/adopt leaves them null (`robot_username` and `robot_secret`), and a refresh nulls them (with a warning) when the registry was disabled and re-enabled outside Terraform.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"robot_secret": schema.StringAttribute{Computed: true, Sensitive: true,
				MarkdownDescription: "The default robot's secret. Known only from the enable call; import/adopt leaves them null (`robot_username` and `robot_secret`), and a refresh nulls them (with a warning) when the registry was disabled and re-enabled outside Terraform. Credentials rotate in the portal, not through this resource.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			// State-only: read by Delete, never sent anywhere else, so an
			// in-place change makes zero API calls (T11-3).
			"purge_on_destroy": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "When `true`, destroying this resource deletes EVERY repository of the registry first (irreversible). Defaults to `false`: the images stay in the platform's project and are restored by a later enable. " +
					"Changing it alone calls no API."},
			"created_at": immutableComputed("Creation time (RFC 3339)."),
			"updated_at": immutableComputed("Time the registry was last written (RFC 3339); no route updates a registry."),
		},
	}
}

// Configure receives the provider's client and organization.
func (r *registryResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// Create enables the registry with ONE bodyless POST. A 201 carries the
// registry and its default robot credential (the only time the password is
// ever returned). A 409 robot_exists means the registry is already enabled -
// or looks it (T11-1): GET decides. Enabled → adopt with null credentials;
// disabled → error (the org's default Harbor robot survived without a
// registry row, aether-api#312), and nothing is written to state.
func (r *registryResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan registryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.pd.Client.EnableRegistryWithResponse(ctx, r.pd.orgID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot enable registry", err.Error())
		return
	}
	switch res.StatusCode() {
	case http.StatusCreated:
		if res.JSON201 == nil || res.JSON201.Registry.Id == "" {
			what := describeNonJSON(res.HTTPResponse, res.Status())
			if res.JSON201 != nil {
				what = describeJSONWithout(res.HTTPResponse, res.Status(), "registry id")
			}
			resp.Diagnostics.AddError("Aether API enabled the registry but returned no usable body",
				what+". The registry was probably enabled, but its credentials are lost: import it (`robot_username`/`robot_secret` stay null) and rotate the credentials in the portal.")
			return
		}
		flattenRegistry(&res.JSON201.Registry, &plan)
		plan.RobotUsername = types.StringValue(res.JSON201.Credentials.Username)
		plan.RobotSecret = types.StringValue(res.JSON201.Credentials.Password)
	case http.StatusConflict:
		apiErr := apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body)
		if apiErr.Code != "robot_exists" {
			apierr.ToDiagnostics(apiErr, &resp.Diagnostics, path.Empty())
			return
		}
		reg, enabled, diags := r.get(ctx)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		if !enabled {
			resp.Diagnostics.AddError("Registry cannot be enabled",
				"The platform reports the registry as disabled but Harbor already holds its robot (`org-<organization slug>`), so every enable is refused with 409 `robot_exists` (aether-api#312). "+
					"Either a previous disable could not delete that robot, or a robot account with that name was created. Delete the robot (an `aether_registry_robot_account` named `org-<slug>`, or in Harbor) and apply again.")
			return
		}
		// Adopt: the credential was minted by someone else's enable call and
		// cannot be read back.
		flattenRegistry(reg, &plan)
		plan.RobotUsername = types.StringNull()
		plan.RobotSecret = types.StringNull()
	default:
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the registry. A disabled registry (the API answers 200
// {"enabled":false}, never 404) is removed from state. robot_username,
// robot_secret and purge_on_destroy are never returned by GET and are carried
// from prior state - EXCEPT when the registry id changed (T11-18, amends
// T11-2): a disable + re-enable outside Terraform deleted the default robot
// and minted a new one under a new registry row, so the carried credentials
// are revoked. They are nulled (the import/adopt shape) with a warning.
func (r *registryResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state registryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	reg, enabled, diags := r.get(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !enabled {
		resp.State.RemoveResource(ctx)
		return
	}
	if !state.ID.IsNull() && state.ID.ValueString() != reg.Id {
		state.RobotUsername = types.StringNull()
		state.RobotSecret = types.StringNull()
		resp.Diagnostics.AddWarning("Registry was re-enabled outside Terraform",
			fmt.Sprintf("The registry id changed from %q to %q: the registry was disabled and re-enabled outside Terraform, which deleted the default robot and minted a new one. "+
				"The enable-time credentials are no longer known, so `robot_username` and `robot_secret` are now null — re-import or re-enable through Terraform to obtain new ones, or rotate them in the portal.",
				state.ID.ValueString(), reg.Id))
	}
	flattenRegistry(reg, &state)
	if state.PurgeOnDestroy.IsNull() {
		state.PurgeOnDestroy = types.BoolValue(false)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update only records the plan: purge_on_destroy is the one attribute that
// can change in place, and it exists only in Terraform (T11-3).
func (r *registryResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan registryModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete disables the registry: DELETE /registry, with ?purge=true when
// purge_on_destroy is set. The API is idempotent (204 also when already
// disabled); anything else is an error and keeps state.
func (r *registryResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state registryModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var params *client.DisableRegistryParams
	if state.PurgeOnDestroy.ValueBool() {
		purge := client.True
		params = &client.DisableRegistryParams{Purge: &purge}
	}
	res, err := r.pd.Client.DisableRegistryWithResponse(ctx, r.pd.orgID, params)
	if err != nil {
		resp.Diagnostics.AddError("Cannot disable registry", err.Error())
		return
	}
	if res.StatusCode() != http.StatusNoContent {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
	}
}

// ImportState imports the organization's registry. The registry is a
// singleton addressed by organization, so the import id is not used to look
// anything up (T11-4): the GET decides. An enabled registry is written in
// full (credentials null: they are known only from the enable call); a
// disabled one is an error rather than an import that Read would silently
// drop.
func (r *registryResource) ImportState(ctx context.Context, _ resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	reg, enabled, diags := r.get(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !enabled {
		resp.Diagnostics.AddError("Cannot import registry",
			"the organization's registry is not enabled. Create the `aether_registry` resource instead of importing it.")
		return
	}
	m := registryModel{
		RobotUsername:  types.StringNull(),
		RobotSecret:    types.StringNull(),
		PurgeOnDestroy: types.BoolValue(false),
	}
	flattenRegistry(reg, &m)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// get reads GET /registry and discriminates the undiscriminated 200 union
// itself (T11-16): the generated AsRegistry() also "succeeds" on
// {"enabled":false}, yielding a zero Registry, so a body is a registry only
// when it says enabled AND carries an id. enabled=false (the only other shape
// the API sends) reports (nil, false).
func (r *registryResource) get(ctx context.Context) (*client.Registry, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	g, err := r.pd.Client.GetRegistryWithResponse(ctx, r.pd.orgID)
	if err != nil {
		diags.AddError("Cannot read registry", err.Error())
		return nil, false, diags
	}
	if g.StatusCode() != http.StatusOK {
		addAPIError(&diags, g.StatusCode(), g.HTTPResponse.Header, g.Body, path.Empty())
		return nil, false, diags
	}
	if g.JSON200 == nil {
		diags.AddError("Aether API returned no usable registry", describeNonJSON(g.HTTPResponse, g.Status()))
		return nil, false, diags
	}
	reg, err := g.JSON200.AsRegistry()
	if err != nil {
		diags.AddError("Aether API returned an unreadable registry", fmt.Sprintf("Cannot decode the registry: %s.", err))
		return nil, false, diags
	}
	if !reg.Enabled {
		return nil, false, diags
	}
	if reg.Id == "" {
		// enabled=true without an id is neither shape the API sends. An
		// error, not a removal (T11-20): a removal would plan a POST that
		// 409s, and failing loudly on an unknown shape is the contract.
		diags.AddError("Aether API returned an unreadable registry",
			fmt.Sprintf("GET %s answered enabled=true without a registry id.", g.HTTPResponse.Request.URL))
		return nil, false, diags
	}
	return &reg, true, diags
}

// flattenRegistry copies an API registry into the model. The credentials and
// purge_on_destroy are not touched here: GET does not return them (Read nulls
// the credentials itself when the registry id changed, T11-18).
func flattenRegistry(reg *client.Registry, m *registryModel) {
	m.ID = types.StringValue(reg.Id)
	m.HarborProject = types.StringValue(reg.HarborProject)
	m.Endpoint = types.StringValue(reg.Endpoint)
	m.Enabled = types.BoolValue(reg.Enabled)
	m.QuotaGB = types.Int64Value(int64(reg.QuotaGb))
	m.CreatedAt = types.StringValue(reg.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(reg.UpdatedAt.Format(time.RFC3339))
}

// describeJSONWithout describes a success answer that WAS decoded as JSON but
// lacks the field that makes it usable (m-5). describeNonJSON is for bodies
// that are not JSON at all; using it here would say "not JSON" about a JSON
// body.
func describeJSONWithout(hr *http.Response, status, missing string) string {
	return fmt.Sprintf("%s %s returned %s with content type %q: JSON without a usable %s",
		hr.Request.Method, hr.Request.URL, status, hr.Header.Get("Content-Type"), missing)
}

// describeUndecodable picks the message for a body the provider decoded
// itself with json.Unmarshal: decodeErr != nil → the body is not (valid)
// JSON; otherwise it was JSON but lacked missing.
func describeUndecodable(hr *http.Response, status string, decodeErr error, missing string) string {
	if decodeErr != nil {
		return fmt.Sprintf("%s (%s)", describeNonJSON(hr, status), decodeErr)
	}
	return describeJSONWithout(hr, status, missing)
}
