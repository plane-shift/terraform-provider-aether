package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
)

var (
	_ resource.Resource                = &robotAccountResource{}
	_ resource.ResourceWithConfigure   = &robotAccountResource{}
	_ resource.ResourceWithImportState = &robotAccountResource{}
)

// robotPageSize is the API's fixed robot-list page size (contract §6): a
// page shorter than this is the last one. `?limit` is ignored by the API.
const robotPageSize = 50

// robotPermissions is the create path's whole vocabulary (contract §6).
var robotPermissions = []string{"push", "pull", "delete"}

// NewRegistryRobotAccountResource returns the aether_registry_robot_account resource.
func NewRegistryRobotAccountResource() resource.Resource { return &robotAccountResource{} }

// robotAccountResource manages one robot account of the organization's
// registry. There is no update route, so every configurable attribute is
// RequiresReplace; there is no GET by id, so Read pages the list. Create
// and Delete are one request each. No timeouts (T11-15).
type robotAccountResource struct{ pd *providerData }

// robotAccountModel is the aether_registry_robot_account state.
type robotAccountModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Permissions    types.Set    `tfsdk:"permissions"`
	Secret         types.String `tfsdk:"secret"`
	HarborUsername types.String `tfsdk:"harbor_username"`
	HarborID       types.Int64  `tfsdk:"harbor_id"`
	CreatedAt      types.String `tfsdk:"created_at"`
	UpdatedAt      types.String `tfsdk:"updated_at"`
}

// robotAccountWire is a robot account as the API sends it. The generated
// RobotAccount/RobotAccountWithSecret types DROP harbor_username, and the
// secret travels as `token` (contract §6), so both the create body and the
// list items are decoded from the raw response into this local type.
type robotAccountWire struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Permissions    []string  `json:"permissions"`
	HarborID       *int64    `json:"harbor_id"`
	HarborUsername *string   `json:"harbor_username"`
	Token          *string   `json:"token"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Metadata names the resource aether_registry_robot_account.
func (r *robotAccountResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry_robot_account"
}

// Schema defines aether_registry_robot_account (T11-6).
func (r *robotAccountResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A robot account (non-expiring `docker login` credential) of the organization's registry. It does not require `aether_registry` to exist. " +
			"The API has no update route, so any change replaces the robot account (a new secret is issued). " +
			"`create_before_destroy` is unsupported: the name is held until the old robot account is deleted, so the replacement is refused with 409. " +
			"Destroying `aether_registry` removes every robot account out of band (they are recreated on the next apply); order destroys with `depends_on` on the registry.",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("Robot account UUID."),
			"name": schema.StringAttribute{Required: true,
				MarkdownDescription: "Robot account name, unique within the organization. Changing it replaces the robot account.",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"permissions": schema.SetAttribute{Required: true, ElementType: types.StringType,
				MarkdownDescription: "Any of `push`, `pull`, `delete`. `push` implies `pull` in the registry; the stored value is what you sent. Changing it replaces the robot account.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.OneOf(robotPermissions...)),
				},
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplace()}},
			"secret": schema.StringAttribute{Computed: true, Sensitive: true,
				MarkdownDescription: "The robot's secret (the `docker login` password). Returned by the API only on creation, so it is null after an import.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"harbor_username": immutableComputed("The `docker login` username (`robot$aether-<organization slug>+<name>`)."),
			"harbor_id": schema.Int64Attribute{Computed: true,
				MarkdownDescription: "The robot's id in the registry backend.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"created_at": immutableComputed("Creation time (RFC 3339)."),
			"updated_at": observedComputed("Time the robot account was last written (RFC 3339)."),
		},
	}
}

// Configure receives the provider's client and organization.
func (r *robotAccountResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// Create POSTs exactly {name, permissions} with the permissions SORTED (the
// API stores them verbatim, so a deterministic body keeps its stored order
// deterministic too). The 201 body is decoded raw for harbor_username and the
// secret (`token`).
func (r *robotAccountResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan robotAccountModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	perms := []string{}
	resp.Diagnostics.Append(plan.Permissions.ElementsAs(ctx, &perms, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	sort.Strings(perms)
	body := client.CreateRobotAccountRequest{Name: plan.Name.ValueString(), Permissions: &perms}
	res, err := r.pd.Client.CreateRobotAccountWithResponse(ctx, r.pd.orgID, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create robot account", err.Error())
		return
	}
	if res.StatusCode() != http.StatusCreated {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	var created robotAccountWire
	if err := json.Unmarshal(res.Body, &created); err != nil || created.ID == "" || created.Token == nil {
		resp.Diagnostics.AddError("Aether API created the robot account but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+" (or no id or secret). The robot account was probably created, but it is not recorded in state and its secret is lost: delete it through the API or the portal and apply again.")
		return
	}
	resp.Diagnostics.Append(flattenRobot(ctx, &created, &plan)...)
	plan.Secret = types.StringValue(*created.Token)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read pages the robot list for the id (T11-7). Absent → removed from state.
// The secret is never listed and is carried from prior state.
func (r *robotAccountResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state robotAccountModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := r.find(ctx, state.ID.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if found == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(flattenRobot(ctx, found, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: every configurable attribute is RequiresReplace and
// there is no update route. It records the plan so the framework contract
// holds if that ever changes.
func (r *robotAccountResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan robotAccountModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the robot account. 204 and 404 (already gone - e.g. the
// registry was disabled, which deletes every robot) are success. Anything
// else - notably 503 when the registry backend could not revoke the
// credential, in which case the API KEEPS the row - is an error and keeps
// state (T11-8).
func (r *robotAccountResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state robotAccountModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.pd.Client.DeleteRobotAccountWithResponse(ctx, r.pd.orgID, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete robot account", err.Error())
		return
	}
	if code := res.StatusCode(); code != http.StatusNoContent && code != http.StatusNotFound {
		addAPIError(&resp.Diagnostics, code, res.HTTPResponse.Header, res.Body, path.Empty())
	}
}

// ImportState imports a robot account by its id (canonical UUID text). The
// lookup is done here rather than left to Read so that the platform's
// default registry robot can be refused (T11-7): its permissions use the
// `repository:*` vocabulary the create path rejects, so managing it here would
// plan a replacement that destroys the registry's credential and then 409s
// on the name. The secret stays null (it is never listed).
func (r *robotAccountResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := parseID(req.ID)
	if err != nil || id.String() != req.ID {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Expected the robot account's id as a canonical lower-case UUID (36 characters), got %q.", req.ID))
		return
	}
	found, diags := r.find(ctx, req.ID)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if found == nil {
		resp.Diagnostics.AddError("Cannot import robot account",
			fmt.Sprintf("No robot account with id %q exists in the organization.", req.ID))
		return
	}
	for _, p := range found.Permissions {
		if !isRobotPermission(p) {
			resp.Diagnostics.AddError("Cannot import robot account",
				fmt.Sprintf("Robot account %q has permission %q: this is the platform's default registry robot; it belongs to aether_registry and cannot be managed here.", found.Name, p))
			return
		}
	}
	m := robotAccountModel{Secret: types.StringNull()}
	resp.Diagnostics.Append(flattenRobot(ctx, found, &m)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// find pages GET /robot-accounts (the only read path, contract §6) until it
// finds id or reaches a short page. The API returns no next_cursor; its
// undocumented `?cursor=<last id>` is added through a RequestEditorFn because
// the generated call takes no parameters. A nil result with no diagnostics
// means the robot does not exist.
func (r *robotAccountResource) find(ctx context.Context, id string) (*robotAccountWire, diag.Diagnostics) {
	var diags diag.Diagnostics
	cursor := ""
	for {
		var editors []client.RequestEditorFn
		if cursor != "" {
			c := cursor
			editors = append(editors, func(_ context.Context, req *http.Request) error {
				q := req.URL.Query()
				q.Set("cursor", c)
				req.URL.RawQuery = q.Encode()
				return nil
			})
		}
		res, err := r.pd.Client.ListRobotAccountsWithResponse(ctx, r.pd.orgID, editors...)
		if err != nil {
			diags.AddError("Cannot list robot accounts", err.Error())
			return nil, diags
		}
		if res.StatusCode() != http.StatusOK {
			addAPIError(&diags, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
			return nil, diags
		}
		var page struct {
			Items *[]robotAccountWire `json:"items"`
		}
		if err := json.Unmarshal(res.Body, &page); err != nil || page.Items == nil {
			diags.AddError("Aether API returned no usable robot account list", describeNonJSON(res.HTTPResponse, res.Status()))
			return nil, diags
		}
		items := *page.Items
		for i := range items {
			if items[i].ID == id {
				return &items[i], diags
			}
		}
		if len(items) < robotPageSize {
			return nil, diags
		}
		next := items[len(items)-1].ID
		if next == cursor {
			// A full page that does not advance would loop forever.
			diags.AddError("Aether API returned an unusable robot account list",
				fmt.Sprintf("Paging with cursor %q returned the same page again.", cursor))
			return nil, diags
		}
		cursor = next
	}
}

func isRobotPermission(p string) bool {
	for _, v := range robotPermissions {
		if p == v {
			return true
		}
	}
	return false
}

// flattenRobot copies an API robot account into the model. Permissions are
// de-duplicated (the API stores what was sent, duplicates included). The
// secret is never touched: only the create answer carries it.
func flattenRobot(ctx context.Context, w *robotAccountWire, m *robotAccountModel) diag.Diagnostics {
	m.ID = types.StringValue(w.ID)
	m.Name = types.StringValue(w.Name)
	seen := make(map[string]bool, len(w.Permissions))
	perms := make([]string, 0, len(w.Permissions))
	for _, p := range w.Permissions {
		if !seen[p] {
			seen[p] = true
			perms = append(perms, p)
		}
	}
	set, diags := types.SetValueFrom(ctx, types.StringType, perms)
	m.Permissions = set
	m.HarborUsername = optString(w.HarborUsername)
	if w.HarborID == nil {
		m.HarborID = types.Int64Null()
	} else {
		m.HarborID = types.Int64Value(*w.HarborID)
	}
	m.CreatedAt = types.StringValue(w.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(w.UpdatedAt.Format(time.RFC3339))
	return diags
}
