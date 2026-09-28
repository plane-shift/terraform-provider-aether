package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &costBreakdownDataSource{}

// NewCostBreakdownDataSource is aether_cost_breakdown (T14-10).
func NewCostBreakdownDataSource() datasource.DataSource { return &costBreakdownDataSource{} }

type costBreakdownDataSource struct{ pd *providerData }

type usageLineModel struct {
	NodePoolID   types.String  `tfsdk:"node_pool_id"`
	PoolName     types.String  `tfsdk:"pool_name"`
	ResourceType types.String  `tfsdk:"resource_type"`
	Unit         types.String  `tfsdk:"unit"`
	Quantity     types.Float64 `tfsdk:"quantity"`
	UnitPriceEUR types.Float64 `tfsdk:"unit_price_eur"`
	AmountEUR    types.Float64 `tfsdk:"amount_eur"`
}

type costBreakdownModel struct {
	PeriodStart   types.String            `tfsdk:"period_start"`
	PeriodEnd     types.String            `tfsdk:"period_end"`
	SubtotalEUR   types.Float64           `tfsdk:"subtotal_eur"`
	Subscriptions []subscriptionLineModel `tfsdk:"subscriptions"`
	Autoscaling   []usageLineModel        `tfsdk:"autoscaling"`
}

func (d *costBreakdownDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cost_breakdown"
}

func (d *costBreakdownDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Month-to-date cost breakdown of the organization, for the current UTC month only (the API offers no other period). " +
			"`autoscaling` mixes two kinds of line: per node pool, one line per resource type (`vcpu`, `memory`, `disk`) with the pool's `node_pool_id` and name; " +
			"and storage lines (`registry`, `storage_block`, `storage_rwx`, in GiB-hours) whose `node_pool_id` is null and whose `pool_name` holds a CLUSTER name (or a label such as `(deleted cluster)`), not a pool. " +
			"The `registry` line is where registry storage consumption appears.",
		Attributes: map[string]schema.Attribute{
			"period_start":  schema.StringAttribute{Computed: true, MarkdownDescription: "First instant of the current UTC month (RFC 3339)."},
			"period_end":    schema.StringAttribute{Computed: true, MarkdownDescription: "First instant of the next UTC month (RFC 3339)."},
			"subtotal_eur":  schema.Float64Attribute{Computed: true, MarkdownDescription: "Month-to-date total in euros (subscriptions plus usage lines)."},
			"subscriptions": subscriptionsAttribute(),
			"autoscaling": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "Usage lines: per-pool resource lines followed by storage lines (see the data source description).",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"node_pool_id":   schema.StringAttribute{Computed: true, MarkdownDescription: "Node pool UUID; null for storage lines and deleted pools."},
					"pool_name":      schema.StringAttribute{Computed: true, MarkdownDescription: "Node pool name for pool lines; the CLUSTER name (or a label) for storage lines."},
					"resource_type":  schema.StringAttribute{Computed: true, MarkdownDescription: "`vcpu`, `memory` or `disk` for pool lines; `registry`, `storage_block` or `storage_rwx` for storage lines. Not an enum: other values may appear."},
					"unit":           schema.StringAttribute{Computed: true, MarkdownDescription: "Unit of `quantity` (e.g. `gib-hour`)."},
					"quantity":       schema.Float64Attribute{Computed: true, MarkdownDescription: "Quantity consumed this month."},
					"unit_price_eur": schema.Float64Attribute{Computed: true, MarkdownDescription: "Price per unit in euros."},
					"amount_eur":     schema.Float64Attribute{Computed: true, MarkdownDescription: "Line cost in euros."},
				}}},
		},
	}
}

func (d *costBreakdownDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *costBreakdownDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	res, err := d.pd.Client.GetCostBreakdownWithResponse(ctx, d.pd.orgID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the cost breakdown", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	if res.JSON200 == nil {
		addNoJSONBodyError(&resp.Diagnostics, res.HTTPResponse, res.Status())
		return
	}
	b := res.JSON200
	out := costBreakdownModel{
		PeriodStart:   types.StringValue(b.PeriodStart.Format(time.RFC3339)),
		PeriodEnd:     types.StringValue(b.PeriodEnd.Format(time.RFC3339)),
		SubtotalEUR:   types.Float64Value(b.SubtotalEur),
		Subscriptions: flattenSubscriptions(b.Subscriptions),
		Autoscaling:   make([]usageLineModel, 0, len(b.Autoscaling)),
	}
	for _, l := range b.Autoscaling {
		out.Autoscaling = append(out.Autoscaling, usageLineModel{
			NodePoolID:   optUUIDString(l.NodePoolId),
			PoolName:     types.StringValue(l.PoolName),
			ResourceType: types.StringValue(string(l.ResourceType)),
			Unit:         types.StringValue(l.Unit),
			Quantity:     types.Float64Value(l.Quantity),
			UnitPriceEUR: types.Float64Value(l.UnitPriceEur),
			AmountEUR:    types.Float64Value(l.AmountEur),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
