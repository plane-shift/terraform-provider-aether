package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

var (
	_ resource.Resource                = &cleanupPolicyResource{}
	_ resource.ResourceWithConfigure   = &cleanupPolicyResource{}
	_ resource.ResourceWithImportState = &cleanupPolicyResource{}
)

// NewRegistryCleanupPolicyResource returns the aether_registry_cleanup_policy resource.
func NewRegistryCleanupPolicyResource() resource.Resource { return &cleanupPolicyResource{} }

// cleanupPolicyResource manages one registry cleanup policy. There is no GET
// by id, so Read lists (unpaginated) and matches the id. Create and Update
// are one request each and ALWAYS send every field (the API applies no
// defaults and its PUT is a full replace, T11-11). Delete is one request,
// plus a list when the API answers anything but 204 (T11-13). No timeouts
// (T11-15).
type cleanupPolicyResource struct{ pd *providerData }

// cleanupPolicyModel is the aether_registry_cleanup_policy state.
type cleanupPolicyModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Action            types.String `tfsdk:"action"`
	Schedule          types.String `tfsdk:"schedule"`
	RepoScope         types.Set    `tfsdk:"repo_scope"`
	Enabled           types.Bool   `tfsdk:"enabled"`
	Conditions        types.List   `tfsdk:"conditions"`
	LastRunAt         types.String `tfsdk:"last_run_at"`
	LastRunDeleted    types.Int64  `tfsdk:"last_run_deleted"`
	LastRunFreedBytes types.Int64  `tfsdk:"last_run_freed_bytes"`
	CreatedAt         types.String `tfsdk:"created_at"`
	UpdatedAt         types.String `tfsdk:"updated_at"`
}

// cleanupConditionModel is one element of `conditions`.
type cleanupConditionModel struct {
	Type  types.String `tfsdk:"type"`
	Op    types.String `tfsdk:"op"`
	Value types.String `tfsdk:"value"`
}

var cleanupConditionAttrTypes = map[string]attr.Type{
	"type":  types.StringType,
	"op":    types.StringType,
	"value": types.StringType,
}

// cleanupPolicyRequest is the create/update body. It is a LOCAL type on
// purpose: the generated CleanupCondition.Op is a non-optional string, but an
// `untagged` condition has no op and the key must be OMITTED (T11-11). Every
// top-level field is always sent (no omitempty): the API 500s on an omitted
// action/schedule/repo_scope and its PUT replaces what is omitted.
type cleanupPolicyRequest struct {
	Name       string                 `json:"name"`
	Action     string                 `json:"action"`
	Conditions []cleanupConditionWire `json:"conditions"`
	RepoScope  []string               `json:"repo_scope"`
	Schedule   string                 `json:"schedule"`
	Enabled    bool                   `json:"enabled"`
}

// cleanupConditionWire is a condition as sent: `value` always a string
// (the portal's convention, and what every condition type parses), op/value
// omitted when null.
type cleanupConditionWire struct {
	Type  string  `json:"type"`
	Op    *string `json:"op,omitempty"`
	Value *string `json:"value,omitempty"`
}

// cleanupPolicyWire is a policy as the API returns it. Decoded raw rather
// than through the generated CleanupPolicy, whose Op string cannot tell an
// absent op from "" and whose Value is untyped anyway.
type cleanupPolicyWire struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Action     string   `json:"action"`
	Schedule   string   `json:"schedule"`
	RepoScope  []string `json:"repo_scope"`
	Enabled    bool     `json:"enabled"`
	Conditions []struct {
		Type  string  `json:"type"`
		Op    *string `json:"op"`
		Value any     `json:"value"`
	} `json:"conditions"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	LastRunAt         *time.Time `json:"last_run_at"`
	LastRunDeleted    *int64     `json:"last_run_deleted"`
	LastRunFreedBytes *int64     `json:"last_run_freed_bytes"`
}

// Metadata names the resource aether_registry_cleanup_policy.
func (r *cleanupPolicyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry_cleanup_policy"
}

// Schema defines aether_registry_cleanup_policy (T11-9).
func (r *cleanupPolicyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A cleanup policy of the organization's registry: artifacts matching ALL `conditions` in the repositories matched by `repo_scope` are deleted (or protected) on `schedule`. " +
			"A policy with `action = \"delete\"`, a `nightly` or `weekly` schedule and `enabled = true` is due immediately and the platform runs it within about a minute of the apply. Test with `action = \"protect\"` or `enabled = false` first. " +
			"`protect` policies are not consulted by the scheduler today (aether-api#309). Policies survive destroying `aether_registry`.",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("Cleanup policy UUID."),
			"name": schema.StringAttribute{Required: true,
				MarkdownDescription: "Policy name (not required to be unique).",
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)}},
			"action": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("delete"),
				MarkdownDescription: "`delete` or `protect`. Defaults to `delete`.",
				Validators:          []validator.String{stringvalidator.OneOf("delete", "protect")}},
			"schedule": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("nightly"),
				MarkdownDescription: "`nightly`, `weekly` or `manual`. Defaults to `nightly`.",
				Validators:          []validator.String{stringvalidator.OneOf("nightly", "weekly", "manual")}},
			"repo_scope": schema.SetAttribute{Optional: true, Computed: true, ElementType: types.StringType,
				Default: setdefault.StaticValue(types.SetValueMust(types.StringType, []attr.Value{})),
				MarkdownDescription: "Repository globs the policy applies to; empty (the default) means all repositories. " +
					"A glob containing `/` matches the full repository name, otherwise the last path segment."},
			"enabled": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				MarkdownDescription: "Whether the scheduler runs the policy. Defaults to `true`."},
			"conditions": schema.ListNestedAttribute{Required: true,
				MarkdownDescription: "Conditions an artifact must ALL satisfy. Per `type`: " +
					"`age` — `op` `gt` or `lt`, `value` `<N>d` or a Go duration (`720h`); " +
					"`tag_count` — `op` `gt` (keep the N newest), `value` a non-negative integer; " +
					"`tag_name` — `op` `eq`, `matches` or `not_matches` (Go regular expression), `value` non-empty; " +
					"`untagged` — no `op`, `value` `\"true\"` or `\"false\"`, REQUIRED: the platform evaluates an untagged condition without a value as false, which matches every TAGGED artifact (it fails open, aether-api#311).",
				Validators: []validator.List{listvalidator.SizeAtLeast(1), cleanupConditionsValidator{}},
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"type": schema.StringAttribute{Required: true,
						MarkdownDescription: "`age`, `tag_count`, `tag_name` or `untagged`.",
						Validators:          []validator.String{stringvalidator.OneOf("age", "tag_count", "tag_name", "untagged")}},
					"op": schema.StringAttribute{Optional: true,
						MarkdownDescription: "Comparison operator; allowed values depend on `type`. Must be omitted for `untagged`."},
					"value": schema.StringAttribute{Optional: true,
						MarkdownDescription: "Operand, always written as a string; the format depends on `type`."},
				}}},
			// No UseStateForUnknown on any of these: runs write them.
			"last_run_at":          observedComputed("Time of the last run (RFC 3339); null until the policy has run."),
			"last_run_deleted":     schema.Int64Attribute{Computed: true, MarkdownDescription: "Artifacts deleted by the last run; null until the policy has run."},
			"last_run_freed_bytes": schema.Int64Attribute{Computed: true, MarkdownDescription: "Bytes freed by the last run; null until the policy has run."},
			"created_at":           immutableComputed("Creation time (RFC 3339)."),
			"updated_at":           observedComputed("Time the policy was last written (RFC 3339); runs move it too."),
		},
	}
}

var (
	ageValueRE      = regexp.MustCompile(`^([0-9]+d|([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+)$`)
	tagCountValueRE = regexp.MustCompile(`^[0-9]+$`)
)

// cleanupConditionsValidator checks each condition against what the
// platform's engine honours for its type (T11-10). The API validates nothing
// (contract §9), so this is stricter than the API on purpose: every refused
// shape is one the engine ignores (matches nothing) or, for `untagged`, one
// that fails OPEN. Unknown elements are skipped (checked again once known);
// an unknown `type` is left to the attribute's OneOf.
type cleanupConditionsValidator struct{}

func (cleanupConditionsValidator) Description(context.Context) string {
	return "each condition's op and value must fit its type"
}

func (v cleanupConditionsValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (cleanupConditionsValidator) ValidateList(ctx context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for i, elem := range req.ConfigValue.Elements() {
		obj, ok := elem.(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() {
			continue
		}
		var c cleanupConditionModel
		if d := obj.As(ctx, &c, basetypes.ObjectAsOptions{}); d.HasError() {
			resp.Diagnostics.Append(d...)
			continue
		}
		if c.Type.IsNull() || c.Type.IsUnknown() || c.Op.IsUnknown() || c.Value.IsUnknown() {
			continue
		}
		p := req.Path.AtListIndex(i)
		checkCleanupCondition(c.Type.ValueString(), c.Op, c.Value, p, &resp.Diagnostics)
	}
}

// checkCleanupCondition is the per-type rule table of T11-10.
func checkCleanupCondition(typ string, op, value types.String, p path.Path, diags *diag.Diagnostics) {
	opPath, valuePath := p.AtName("op"), p.AtName("value")
	requireOp := func(allowed ...string) {
		if op.IsNull() {
			diags.AddAttributeError(opPath, "Missing condition op",
				fmt.Sprintf("A condition of type %q requires op, one of: %q.", typ, allowed))
			return
		}
		for _, a := range allowed {
			if op.ValueString() == a {
				return
			}
		}
		diags.AddAttributeError(opPath, "Invalid condition op",
			fmt.Sprintf("A condition of type %q accepts op %q, got %q; the platform ignores any other op and the policy matches nothing.", typ, allowed, op.ValueString()))
	}
	switch typ {
	case "age":
		requireOp("gt", "lt")
		if value.IsNull() || !ageValueRE.MatchString(value.ValueString()) {
			diags.AddAttributeError(valuePath, "Invalid condition value",
				fmt.Sprintf("A condition of type %q requires value as `<N>d` (e.g. \"30d\") or a Go duration (e.g. \"720h\"), got %s.", typ, describeValue(value)))
		}
	case "tag_count":
		requireOp("gt")
		if value.IsNull() || !tagCountValueRE.MatchString(value.ValueString()) {
			diags.AddAttributeError(valuePath, "Invalid condition value",
				fmt.Sprintf("A condition of type %q requires value as a non-negative integer (e.g. \"10\"), got %s.", typ, describeValue(value)))
		}
	case "tag_name":
		requireOp("eq", "matches", "not_matches")
		if value.IsNull() || value.ValueString() == "" {
			diags.AddAttributeError(valuePath, "Invalid condition value",
				fmt.Sprintf("A condition of type %q requires a non-empty value.", typ))
			return
		}
		if o := op.ValueString(); o == "matches" || o == "not_matches" {
			if _, err := regexp.Compile(value.ValueString()); err != nil {
				diags.AddAttributeError(valuePath, "Invalid condition value",
					fmt.Sprintf("A condition of type %q with op %q requires value to be a valid Go regular expression: %s.", typ, o, err))
			}
		}
	case "untagged":
		if !op.IsNull() {
			diags.AddAttributeError(opPath, "Invalid condition op",
				fmt.Sprintf("A condition of type %q takes no op; remove op (got %q).", typ, op.ValueString()))
		}
		if value.IsNull() || (value.ValueString() != "true" && value.ValueString() != "false") {
			diags.AddAttributeError(valuePath, "Invalid condition value",
				fmt.Sprintf("A condition of type %q requires value \"true\" or \"false\", got %s. Without a readable value the platform evaluates the condition as false, "+
					"which matches every TAGGED artifact: the policy fails open (aether-api#311).", typ, describeValue(value)))
		}
	}
}

func describeValue(v types.String) string {
	if v.IsNull() {
		return "no value"
	}
	return strconv.Quote(v.ValueString())
}

// Configure receives the provider's client and organization.
func (r *cleanupPolicyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// Create POSTs the full body. A new policy has never run, so last_run_* are
// null (the create answer never carries them anyway); no re-read.
func (r *cleanupPolicyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan cleanupPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, diags := cleanupRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.pd.Client.CreateCleanupPolicyWithBodyWithResponse(ctx, r.pd.orgID, "application/json", bytes.NewReader(body))
	if err != nil {
		resp.Diagnostics.AddError("Cannot create cleanup policy", err.Error())
		return
	}
	if res.StatusCode() != http.StatusCreated {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	var created cleanupPolicyWire
	if err := json.Unmarshal(res.Body, &created); err != nil || created.ID == "" {
		resp.Diagnostics.AddError("Aether API created the cleanup policy but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+" (or no id). The policy was probably created but is not recorded in state: delete it through the API or the portal, or import it by its id.")
		return
	}
	resp.Diagnostics.Append(flattenCleanupPolicy(ctx, &created, &plan)...)
	plan.LastRunAt = types.StringNull()
	plan.LastRunDeleted = types.Int64Null()
	plan.LastRunFreedBytes = types.Int64Null()
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read lists the organization's policies and matches the id (T11-12);
// absent → removed from state.
func (r *cleanupPolicyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state cleanupPolicyModel
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
	resp.Diagnostics.Append(flattenCleanupPolicy(ctx, found, &state)...)
	state.LastRunAt = types.StringNull()
	if found.LastRunAt != nil {
		state.LastRunAt = types.StringValue(found.LastRunAt.Format(time.RFC3339))
	}
	state.LastRunDeleted = optInt64(found.LastRunDeleted)
	state.LastRunFreedBytes = optInt64(found.LastRunFreedBytes)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update PUTs the full body (the API replaces every field but `enabled`,
// T11-11). The answer never carries last_run_*, so they are carried from
// prior state.
func (r *cleanupPolicyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state cleanupPolicyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, diags := cleanupRequestBody(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.pd.Client.UpdateCleanupPolicyWithBodyWithResponse(ctx, r.pd.orgID, state.ID.ValueString(), "application/json", bytes.NewReader(body))
	if err != nil {
		resp.Diagnostics.AddError("Cannot update cleanup policy", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		// An unknown id is a 500 here, not a 404 (aether-api#311): the next
		// refresh removes a policy that is really gone.
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	var updated cleanupPolicyWire
	if err := json.Unmarshal(res.Body, &updated); err != nil || updated.ID == "" {
		resp.Diagnostics.AddError("Aether API accepted the cleanup policy update but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+" (or no id). The update was probably applied, but state still holds the previous values: apply again (the update is idempotent).")
		return
	}
	resp.Diagnostics.Append(flattenCleanupPolicy(ctx, &updated, &plan)...)
	plan.LastRunAt = state.LastRunAt
	plan.LastRunDeleted = state.LastRunDeleted
	plan.LastRunFreedBytes = state.LastRunFreedBytes
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the policy. 204 is success. The API answers 500 (never 404)
// for an id that is already gone (aether-api#311), so on ANY other status the
// policy list decides (T11-13): absent → success, present → the DELETE's
// error, state kept.
func (r *cleanupPolicyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state cleanupPolicyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	res, err := r.pd.Client.DeleteCleanupPolicyWithResponse(ctx, r.pd.orgID, id)
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete cleanup policy", err.Error())
		return
	}
	if res.StatusCode() == http.StatusNoContent {
		return
	}
	found, diags := r.find(ctx, id)
	if diags.HasError() {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		resp.Diagnostics.Append(diags...)
		return
	}
	if found == nil {
		return // already gone
	}
	addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
}

// ImportState imports a policy by its id (canonical UUID text); Read fills
// in the rest from the list.
func (r *cleanupPolicyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := parseID(req.ID)
	if err != nil || id.String() != req.ID {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Expected the cleanup policy's id as a canonical lower-case UUID (36 characters), got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// find lists the organization's policies (unpaginated, contract §10) and
// returns the one with id, or nil when it does not exist.
func (r *cleanupPolicyResource) find(ctx context.Context, id string) (*cleanupPolicyWire, diag.Diagnostics) {
	var diags diag.Diagnostics
	res, err := r.pd.Client.ListCleanupPoliciesWithResponse(ctx, r.pd.orgID)
	if err != nil {
		diags.AddError("Cannot list cleanup policies", err.Error())
		return nil, diags
	}
	if res.StatusCode() != http.StatusOK {
		addAPIError(&diags, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return nil, diags
	}
	var list struct {
		Items *[]cleanupPolicyWire `json:"items"`
	}
	if err := json.Unmarshal(res.Body, &list); err != nil || list.Items == nil {
		diags.AddError("Aether API returned no usable cleanup policy list", describeNonJSON(res.HTTPResponse, res.Status()))
		return nil, diags
	}
	for i := range *list.Items {
		if (*list.Items)[i].ID == id {
			return &(*list.Items)[i], diags
		}
	}
	return nil, diags
}

// cleanupRequestBody renders the full create/update body from the plan:
// every field present, repo_scope sorted and `[]` when empty, condition
// op/value omitted when null.
func cleanupRequestBody(ctx context.Context, plan *cleanupPolicyModel) ([]byte, diag.Diagnostics) {
	var diags diag.Diagnostics
	scope := []string{}
	diags.Append(plan.RepoScope.ElementsAs(ctx, &scope, false)...)
	sort.Strings(scope)
	var conds []cleanupConditionModel
	diags.Append(plan.Conditions.ElementsAs(ctx, &conds, false)...)
	if diags.HasError() {
		return nil, diags
	}
	body := cleanupPolicyRequest{
		Name: plan.Name.ValueString(), Action: plan.Action.ValueString(), Schedule: plan.Schedule.ValueString(),
		Enabled: plan.Enabled.ValueBool(), RepoScope: scope, Conditions: make([]cleanupConditionWire, 0, len(conds)),
	}
	for _, c := range conds {
		body.Conditions = append(body.Conditions, cleanupConditionWire{
			Type: c.Type.ValueString(), Op: c.Op.ValueStringPointer(), Value: c.Value.ValueStringPointer(),
		})
	}
	b, err := json.Marshal(body)
	if err != nil {
		diags.AddError("Cannot encode cleanup policy", err.Error())
		return nil, diags
	}
	return b, diags
}

// flattenCleanupPolicy copies an API policy into the model, last_run_*
// excepted (only the list carries them; callers decide). Condition values
// are normalised to strings (T11-12) because other clients may store numbers
// or booleans; an absent or null op/value reads as null. Element order is
// kept. A null `conditions` reads as an empty list.
func flattenCleanupPolicy(ctx context.Context, p *cleanupPolicyWire, m *cleanupPolicyModel) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(p.ID)
	m.Name = types.StringValue(p.Name)
	m.Action = types.StringValue(p.Action)
	m.Schedule = types.StringValue(p.Schedule)
	m.Enabled = types.BoolValue(p.Enabled)
	scope := p.RepoScope
	if scope == nil {
		scope = []string{}
	}
	set, d := types.SetValueFrom(ctx, types.StringType, scope)
	diags.Append(d...)
	m.RepoScope = set
	conds := make([]cleanupConditionModel, 0, len(p.Conditions))
	for _, c := range p.Conditions {
		conds = append(conds, cleanupConditionModel{
			Type:  types.StringValue(c.Type),
			Op:    optString(c.Op),
			Value: conditionValueString(c.Value),
		})
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: cleanupConditionAttrTypes}, conds)
	diags.Append(d...)
	m.Conditions = list
	m.CreatedAt = types.StringValue(p.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(p.UpdatedAt.Format(time.RFC3339))
	return diags
}

// conditionValueString normalises a stored condition value (arbitrary JSON)
// to the string form the provider sends: float64 → shortest decimal, bool →
// "true"/"false", nil → null. Any other JSON (an object or array only another
// client could have stored) is kept as its compact JSON text, so it shows up
// as a diff against the configuration instead of disappearing.
func conditionValueString(v any) types.String {
	switch x := v.(type) {
	case nil:
		return types.StringNull()
	case string:
		return types.StringValue(x)
	case float64:
		return types.StringValue(strconv.FormatFloat(x, 'f', -1, 64))
	case bool:
		return types.StringValue(strconv.FormatBool(x))
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return types.StringValue(fmt.Sprint(x))
		}
		return types.StringValue(string(b))
	}
}

// optInt64 maps an optional API integer onto a Terraform value (nil → null).
func optInt64(p *int64) types.Int64 {
	if p == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*p)
}
