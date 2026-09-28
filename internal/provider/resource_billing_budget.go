package provider

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
)

var (
	_ resource.Resource                = &billingBudgetResource{}
	_ resource.ResourceWithConfigure   = &billingBudgetResource{}
	_ resource.ResourceWithImportState = &billingBudgetResource{}
	_ resource.ResourceWithModifyPlan  = &billingBudgetResource{}
)

// Limits of monthly_limit_eur. The platform stores the limit as
// NUMERIC(12,2): a value that rounds to 0.00 violates its CHECK > 0 and a
// value at or above 9999999999.995 overflows the column - both answer 500,
// not 422 - so the provider refuses them at plan time instead.
const (
	budgetLimitMin     = 0.01
	budgetLimitCeiling = 10000000000.0 // exclusive
)

// NewBillingBudgetResource returns the aether_billing_budget resource.
func NewBillingBudgetResource() resource.Resource { return &billingBudgetResource{} }

// billingBudgetResource manages the provider organization's spend budget, a
// per-organization SINGLETON. Unlike the cluster network policy it can be
// absent: GET answers 404 until the first PUT and again after a DELETE.
// Every operation is a single request (Create: a GET, then one PUT); nothing
// is waited for and there is no timeouts block (T12-7).
type billingBudgetResource struct{ pd *providerData }

// billingBudgetModel is the aether_billing_budget state; tfsdk tags are the
// attribute names.
type billingBudgetModel struct {
	ID              types.String  `tfsdk:"id"`
	MonthlyLimitEur types.Float64 `tfsdk:"monthly_limit_eur"`
	ThresholdsPct   types.List    `tfsdk:"thresholds_pct"`
	CreatedAt       types.String  `tfsdk:"created_at"`
	UpdatedAt       types.String  `tfsdk:"updated_at"`
}

// Metadata names the resource aether_billing_budget.
func (r *billingBudgetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_billing_budget"
}

// Schema defines aether_billing_budget.
func (r *billingBudgetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		// T12-6: verbatim.
		MarkdownDescription: "The budget is advisory: nothing is blocked when it is exceeded. The platform evaluates spend about every five minutes and sends ONE `usage.alert` webhook per threshold per organization per UTC calendar month. Thresholds already reached this month are not re-armed by changing the limit or by destroying and recreating the budget; they re-arm at the start of the next month. Any organization member or API token can change the budget (aether-api #310).",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("The organization's UUID (the budget has no id of its own)."),
			"monthly_limit_eur": schema.Float64Attribute{Required: true,
				MarkdownDescription: "Monthly spend limit in euros, at most two decimals; the platform stores cents. At least `0.01` and less than `10000000000`.",
				Validators:          []validator.Float64{budgetLimitValidator{}}},
			// A List, not a Set: the API requires and echoes the thresholds in
			// strictly ascending order, so the configured order is the contract.
			"thresholds_pct": schema.ListAttribute{Required: true, ElementType: types.Int64Type,
				MarkdownDescription: "Percentages of the limit (each 1-100, strictly ascending, at least one) at which a `usage.alert` webhook is sent, e.g. `[50, 80, 100]`.",
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.ValueInt64sAre(int64validator.Between(1, 100)),
					ascendingInt64sValidator{},
				}},
			"created_at": immutableComputed("Time the budget was first set (RFC 3339)."),
			// No UseStateForUnknown: the API moves it on EVERY PUT, so an update
			// must plan it unknown (T12-5).
			"updated_at": observedComputed("Time the budget was last written (RFC 3339)."),
		},
	}
}

// budgetLimitValidator enforces T12-2 at plan time: >= 0.01, < 1e10 and at
// most two decimal places. The API would round a sub-cent value (or 500 on
// one below half a cent), and a rounded echo would fail the apply with
// "inconsistent result", so such values are refused before any request.
type budgetLimitValidator struct{}

func (budgetLimitValidator) Description(context.Context) string {
	return "must be at least 0.01, less than 10000000000, with at most two decimal places"
}

func (v budgetLimitValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (budgetLimitValidator) ValidateFloat64(_ context.Context, req validator.Float64Request, resp *validator.Float64Response) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	v := req.ConfigValue.ValueFloat64()
	s := strconv.FormatFloat(v, 'f', -1, 64)
	switch {
	case v < budgetLimitMin:
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid monthly_limit_eur",
			fmt.Sprintf("%s must be at least 0.01 (one cent).", s))
	case v >= budgetLimitCeiling:
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid monthly_limit_eur",
			fmt.Sprintf("%s must be less than 10000000000 (the platform stores at most 9999999999.99).", s))
	case decimalPlaces(s) > 2:
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid monthly_limit_eur",
			fmt.Sprintf("%s has more than two decimal places; the platform stores cents, so write at most two decimals.", s))
	}
}

// decimalPlaces counts the fractional digits of s, the shortest decimal text
// that round-trips to the float64 (strconv 'f', -1). A value such as 12.3 is
// "12.3" here even though its binary representation is inexact.
func decimalPlaces(s string) int {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

// ascendingInt64sValidator mirrors the API's 422 "thresholds must be strictly
// ascending" (which covers duplicates too) at plan time. Unknown elements are
// skipped; Terraform re-validates the resolved list before apply.
type ascendingInt64sValidator struct{}

func (ascendingInt64sValidator) Description(context.Context) string {
	return "thresholds must be strictly ascending"
}

func (v ascendingInt64sValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (ascendingInt64sValidator) ValidateList(_ context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	var prev *int64
	for i, e := range req.ConfigValue.Elements() {
		n, ok := e.(types.Int64)
		if !ok || n.IsNull() || n.IsUnknown() {
			prev = nil
			continue
		}
		cur := n.ValueInt64()
		if prev != nil && cur <= *prev {
			resp.Diagnostics.AddAttributeError(req.Path.AtListIndex(i), "Invalid thresholds_pct",
				fmt.Sprintf("thresholds must be strictly ascending: %d does not exceed the previous threshold %d (duplicates are not allowed either).", cur, *prev))
			return
		}
		prev = &cur
	}
}

// Configure receives the provider's client and organization.
func (r *billingBudgetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan marks updated_at unknown on every update: the API moves it on
// every PUT, and Update PUTs whenever plan and state differ.
func (r *billingBudgetResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy
	}
	if !req.Plan.Raw.Equal(req.State.Raw) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("updated_at"), types.StringUnknown())...)
	}
}

// Create GETs first and refuses to overwrite a budget that is already set
// (T12-4): the API's PUT is an upsert that would silently replace a budget
// set outside Terraform. Only on a 404 does it send ONE PUT with both fields.
func (r *billingBudgetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan billingBudgetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	g, err := r.pd.Client.GetBudgetWithResponse(ctx, r.pd.orgID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot check for an existing billing budget", err.Error())
		return
	}
	switch g.StatusCode() {
	case http.StatusNotFound:
		// The expected case: no budget set yet.
	case http.StatusOK:
		resp.Diagnostics.AddError("A billing budget already exists",
			"a budget already exists for this organization (set outside Terraform); import it: terraform import aether_billing_budget.<name> "+r.pd.OrgID)
		return
	default:
		addAPIError(&resp.Diagnostics, g.StatusCode(), g.HTTPResponse.Header, g.Body, path.Empty())
		return
	}
	resp.Diagnostics.Append(r.put(ctx, &plan, true)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the budget. 404 means it was deleted outside Terraform and
// removes the resource from state; every other failure keeps state.
func (r *billingBudgetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state billingBudgetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	b, found, diags := r.get(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, b, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update replaces the budget with ONE PUT carrying both fields (the API has
// no partial update). The framework calls it only when plan and state differ.
func (r *billingBudgetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan billingBudgetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.put(ctx, &plan, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the budget. 204 is success, and so is 404 (already removed
// outside Terraform: the API's DELETE is not idempotent). Anything else is an
// error and keeps state.
func (r *billingBudgetResource) Delete(ctx context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	res, err := r.pd.Client.DeleteBudgetWithResponse(ctx, r.pd.orgID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete billing budget", err.Error())
		return
	}
	if code := res.StatusCode(); code != http.StatusNoContent && code != http.StatusNotFound {
		addAPIError(&resp.Diagnostics, code, res.HTTPResponse.Header, res.Body, path.Empty())
	}
}

// ImportState adopts the organization's budget. The budget has no id of its
// own, so ANY import id is accepted and the provider's organization is the
// one imported (T12-1); the budget must exist.
func (r *billingBudgetResource) ImportState(ctx context.Context, _ resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	b, found, diags := r.get(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.Diagnostics.AddError("Cannot import billing budget",
			"no budget is set for this organization ("+r.pd.OrgID+"); there is nothing to import - declare the resource and apply to create one.")
		return
	}
	var m billingBudgetModel
	resp.Diagnostics.Append(r.flatten(ctx, b, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// get reads the budget: (budget, true) on 200, (nil, false) on 404, and
// diagnostics for anything else.
func (r *billingBudgetResource) get(ctx context.Context) (*client.Budget, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	g, err := r.pd.Client.GetBudgetWithResponse(ctx, r.pd.orgID)
	if err != nil {
		diags.AddError("Cannot read billing budget", err.Error())
		return nil, false, diags
	}
	switch {
	case g.StatusCode() == http.StatusNotFound:
		return nil, false, diags
	case g.StatusCode() != http.StatusOK:
		addAPIError(&diags, g.StatusCode(), g.HTTPResponse.Header, g.Body, path.Empty())
		return nil, false, diags
	case g.JSON200 == nil:
		diags.AddError("Aether API returned no usable billing budget", describeNonJSON(g.HTTPResponse, g.Status()))
		return nil, false, diags
	}
	return g.JSON200, true, diags
}

// put sends m's limit and thresholds as the whole budget (full replace) and
// flattens the answer - the stored row - into m.
func (r *billingBudgetResource) put(ctx context.Context, m *billingBudgetModel, creating bool) diag.Diagnostics {
	var diags diag.Diagnostics
	var th []int64
	diags.Append(m.ThresholdsPct.ElementsAs(ctx, &th, false)...)
	if diags.HasError() {
		return diags
	}
	body := client.SetBudgetRequest{MonthlyLimitEur: m.MonthlyLimitEur.ValueFloat64(), ThresholdsPct: make([]int, len(th))}
	for i, v := range th {
		body.ThresholdsPct[i] = int(v)
	}
	res, err := r.pd.Client.SetBudgetWithResponse(ctx, r.pd.orgID, body)
	if err != nil {
		diags.AddError("Cannot set billing budget", err.Error())
		return diags
	}
	if res.StatusCode() != http.StatusOK {
		addAPIError(&diags, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return diags
	}
	if res.JSON200 == nil {
		advice := " The budget was probably written, but it is not recorded in state: import it (terraform import aether_billing_budget.<name> " + r.pd.OrgID + ")."
		if !creating {
			advice = " The budget was probably written, but state still holds the previous budget: apply again."
		}
		diags.AddError("Aether API accepted the billing budget but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+"."+advice)
		return diags
	}
	diags.Append(r.flatten(ctx, res.JSON200, m)...)
	return diags
}

// flatten copies an API budget into m. id is the provider's organization id
// (T12-1); the limit comes back rounded to cents, which the plan-time
// validator guarantees equals the configured value.
func (r *billingBudgetResource) flatten(ctx context.Context, b *client.Budget, m *billingBudgetModel) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(r.pd.OrgID)
	m.MonthlyLimitEur = types.Float64Value(b.MonthlyLimitEur)
	th := make([]int64, len(b.ThresholdsPct))
	for i, v := range b.ThresholdsPct {
		th[i] = int64(v)
	}
	v, d := types.ListValueFrom(ctx, types.Int64Type, th)
	diags.Append(d...)
	m.ThresholdsPct = v
	m.CreatedAt = types.StringValue(b.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(b.UpdatedAt.Format(time.RFC3339))
	return diags
}
