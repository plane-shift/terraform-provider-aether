package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &clusterNetworkDataSource{}

// NewClusterNetworkDataSource returns data.aether_cluster_network.
func NewClusterNetworkDataSource() datasource.DataSource { return &clusterNetworkDataSource{} }

type clusterNetworkDataSource struct{ pd *providerData }

type clusterNetworkDataModel struct {
	ClusterID          types.String `tfsdk:"cluster_id"`
	AuthorizedCidrs    types.List   `tfsdk:"authorized_cidrs"`
	ApiServerRateLimit types.Object `tfsdk:"api_server_rate_limit"`
	UpdatedAt          types.String `tfsdk:"updated_at"`
}

func (d *clusterNetworkDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_network"
}

func (d *clusterNetworkDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The API-server network policy of a cluster. An empty `authorized_cidrs` list means the endpoint accepts every source address. " +
			"A cluster whose policy was never configured reads an empty list, a null `api_server_rate_limit` (the platform default) and the zero time as `updated_at`.",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{Required: true, Validators: []validator.String{canonicalIDValidator{}}, MarkdownDescription: "UUID of the cluster."},
			"authorized_cidrs": schema.ListAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Source CIDRs allowed to reach the API server; empty = every source."},
			"api_server_rate_limit": schema.SingleNestedAttribute{Computed: true,
				MarkdownDescription: "Connection rate limit on the API server; null when the platform default applies.",
				Attributes: map[string]schema.Attribute{
					"max_connections_per_second": schema.Int64Attribute{Computed: true, MarkdownDescription: "Sustained new connections per second."},
					"burst":                      schema.Int64Attribute{Computed: true, MarkdownDescription: "Burst allowance."},
				}},
			"updated_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Last update time (RFC 3339); the zero time when never configured."},
		},
	}
}

func (d *clusterNetworkDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterNetworkDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg clusterNetworkDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, ok := parseInputID(path.Root("cluster_id"), cfg.ClusterID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	res, err := d.pd.Client.GetClusterNetworkWithResponse(ctx, d.pd.orgID, clusterID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the cluster network policy", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	if res.JSON200 == nil {
		addNonJSONError(&resp.Diagnostics, res.HTTPResponse, res.Status())
		return
	}
	var m clusterNetworkModel
	resp.Diagnostics.Append(flattenClusterNetwork(ctx, res.JSON200, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out := clusterNetworkDataModel{ClusterID: cfg.ClusterID, AuthorizedCidrs: m.AuthorizedCidrs, ApiServerRateLimit: m.ApiServerRateLimit, UpdatedAt: m.UpdatedAt}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
