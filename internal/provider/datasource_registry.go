package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSourceWithConfigure = &registryDataSource{}

// NewRegistryDataSource is aether_registry (T14-3).
func NewRegistryDataSource() datasource.DataSource { return &registryDataSource{} }

type registryDataSource struct{ pd *providerData }

type registryDataModel struct {
	Enabled       types.Bool   `tfsdk:"enabled"`
	ID            types.String `tfsdk:"id"`
	HarborProject types.String `tfsdk:"harbor_project"`
	Endpoint      types.String `tfsdk:"endpoint"`
	QuotaGB       types.Int64  `tfsdk:"quota_gb"`
	CreatedAt     types.String `tfsdk:"created_at"`
	UpdatedAt     types.String `tfsdk:"updated_at"`
}

func (d *registryDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry"
}

func (d *registryDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The organization's container registry. A registry that is not enabled reads `enabled = false` with every other attribute null — it is not an error, so the data source can drive conditional configuration. " +
			"Storage consumption is not reported here: see the `registry` line of `aether_cost_breakdown`'s `autoscaling` list (GiB-hours and euros).",
		Attributes: map[string]schema.Attribute{
			"enabled":        schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the registry is enabled."},
			"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "Registry id; null when not enabled. It changes when the registry is disabled and enabled again."},
			"harbor_project": schema.StringAttribute{Computed: true, MarkdownDescription: "Registry project name (`aether-<organization slug>`); null when not enabled."},
			"endpoint":       schema.StringAttribute{Computed: true, MarkdownDescription: "Registry host to push to and pull from; null when not enabled."},
			"quota_gb":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Storage quota in GB as recorded by the platform. It is not enforced today. Null when not enabled."},
			"created_at":     schema.StringAttribute{Computed: true, MarkdownDescription: "Time the registry was enabled (RFC 3339); null when not enabled."},
			"updated_at":     schema.StringAttribute{Computed: true, MarkdownDescription: "Last write time (RFC 3339); null when not enabled."},
		},
	}
}

func (d *registryDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *registryDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	reg, enabled, diags := getRegistry(ctx, d.pd)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	out := registryDataModel{
		Enabled: types.BoolValue(false), ID: types.StringNull(), HarborProject: types.StringNull(),
		Endpoint: types.StringNull(), QuotaGB: types.Int64Null(), CreatedAt: types.StringNull(), UpdatedAt: types.StringNull(),
	}
	if enabled {
		var m registryModel
		flattenRegistry(reg, &m)
		out = registryDataModel{
			Enabled: m.Enabled, ID: m.ID, HarborProject: m.HarborProject, Endpoint: m.Endpoint,
			QuotaGB: m.QuotaGB, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
