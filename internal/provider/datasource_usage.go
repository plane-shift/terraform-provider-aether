package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &usageDataSource{}

// NewUsageDataSource is aether_usage (T14-9).
func NewUsageDataSource() datasource.DataSource { return &usageDataSource{} }

type usageDataSource struct{ pd *providerData }

// subscriptionLineModel is one flat monthly charge; shared with
// aether_cost_breakdown. Money is euros (Float64, T14-13).
type subscriptionLineModel struct {
	Kind           types.String  `tfsdk:"kind"`
	ClusterID      types.String  `tfsdk:"cluster_id"`
	NodePoolID     types.String  `tfsdk:"node_pool_id"`
	Name           types.String  `tfsdk:"name"`
	AmountEUR      types.Float64 `tfsdk:"amount_eur"`
	GrossAmountEUR types.Float64 `tfsdk:"gross_amount_eur"`
	DiscountEUR    types.Float64 `tfsdk:"discount_eur"`
}

// usageModel deliberately has no `items` (T14-9): the API's items are every
// hourly usage record of the month, unaggregated and unbounded;
// aether_cost_breakdown carries the aggregation.
type usageModel struct {
	PeriodStart   types.String            `tfsdk:"period_start"`
	PeriodEnd     types.String            `tfsdk:"period_end"`
	TotalEUR      types.Float64           `tfsdk:"total_eur"`
	Subscriptions []subscriptionLineModel `tfsdk:"subscriptions"`
}

// subscriptionsAttribute is the `subscriptions` schema shared by
// aether_usage and aether_cost_breakdown.
func subscriptionsAttribute() schema.ListNestedAttribute {
	return schema.ListNestedAttribute{Computed: true,
		MarkdownDescription: "Flat monthly charges of the month (fixed pools, control plane, packages, public endpoints, load balancers).",
		NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"kind":             schema.StringAttribute{Computed: true, MarkdownDescription: "Which flat monthly charge this line is."},
			"cluster_id":       schema.StringAttribute{Computed: true, MarkdownDescription: "Cluster UUID; null when the line is not cluster-scoped."},
			"node_pool_id":     schema.StringAttribute{Computed: true, MarkdownDescription: "Node pool UUID; null when the line is not pool-scoped."},
			"name":             schema.StringAttribute{Computed: true, MarkdownDescription: "Display name of the charged item."},
			"amount_eur":       schema.Float64Attribute{Computed: true, MarkdownDescription: "Net amount in euros (`gross_amount_eur - discount_eur`)."},
			"gross_amount_eur": schema.Float64Attribute{Computed: true, MarkdownDescription: "Amount before discount, in euros."},
			"discount_eur":     schema.Float64Attribute{Computed: true, MarkdownDescription: "Discount in euros."},
		}}}
}

func flattenSubscriptions(lines []client.SubscriptionLine) []subscriptionLineModel {
	out := make([]subscriptionLineModel, 0, len(lines))
	for _, l := range lines {
		out = append(out, subscriptionLineModel{
			Kind:           types.StringValue(string(l.Kind)),
			ClusterID:      optUUIDString(l.ClusterId),
			NodePoolID:     optUUIDString(l.NodePoolId),
			Name:           types.StringValue(l.Name),
			AmountEUR:      types.Float64Value(l.AmountEur),
			GrossAmountEUR: types.Float64Value(l.GrossAmountEur),
			DiscountEUR:    types.Float64Value(l.DiscountEur),
		})
	}
	return out
}

func (d *usageDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_usage"
}

func (d *usageDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Month-to-date spend of the organization: the current UTC month only; the API offers no other period. " +
			"Per-pool and storage usage lines are in `aether_cost_breakdown`.",
		Attributes: map[string]schema.Attribute{
			"period_start":  schema.StringAttribute{Computed: true, MarkdownDescription: "First instant of the current UTC month (RFC 3339)."},
			"period_end":    schema.StringAttribute{Computed: true, MarkdownDescription: "First instant of the next UTC month (RFC 3339)."},
			"total_eur":     schema.Float64Attribute{Computed: true, MarkdownDescription: "Month-to-date total in euros (subscriptions plus usage)."},
			"subscriptions": subscriptionsAttribute(),
		},
	}
}

func (d *usageDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// usageWire is the part of GET /billing/usage the data source reads. The
// body is decoded here rather than through the generated UsageSummary so the
// dropped `items` (every hourly usage record of the month, unbounded) are
// neither decoded nor able to fail the read (T14-9).
type usageWire struct {
	PeriodStart   *time.Time                 `json:"period_start"`
	PeriodEnd     *time.Time                 `json:"period_end"`
	TotalEUR      *float64                   `json:"total_eur"`
	Subscriptions *[]client.SubscriptionLine `json:"subscriptions"`
}

func (d *usageDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	// GetUsageWithResponse would decode the whole body (items included) and
	// fail on it; the embedded plain GetUsage returns the response undecoded.
	hr, err := d.pd.Client.GetUsage(ctx, d.pd.orgID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read usage", err.Error())
		return
	}
	defer hr.Body.Close()
	body, err := io.ReadAll(hr.Body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read usage", err.Error())
		return
	}
	if hr.StatusCode != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(hr.StatusCode, hr.Header, body), &resp.Diagnostics, path.Empty())
		return
	}
	var u usageWire
	err = json.Unmarshal(body, &u)
	if err != nil || u.PeriodStart == nil || u.PeriodEnd == nil || u.TotalEUR == nil || u.Subscriptions == nil {
		resp.Diagnostics.AddError("Aether API returned no usable usage summary",
			describeUndecodable(hr, hr.Status, err, "period_start, period_end, total_eur or subscriptions"))
		return
	}
	out := usageModel{
		PeriodStart:   types.StringValue(u.PeriodStart.Format(time.RFC3339)),
		PeriodEnd:     types.StringValue(u.PeriodEnd.Format(time.RFC3339)),
		TotalEUR:      types.Float64Value(*u.TotalEUR),
		Subscriptions: flattenSubscriptions(*u.Subscriptions),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
