package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &nodePoolsDataSource{}

// NewNodePoolsDataSource returns data.aether_node_pools.
func NewNodePoolsDataSource() datasource.DataSource { return &nodePoolsDataSource{} }

type nodePoolsDataSource struct{ pd *providerData }

type nodePoolsModel struct {
	ClusterID types.String        `tfsdk:"cluster_id"`
	NodePools []nodePoolDataModel `tfsdk:"node_pools"`
}

func (d *nodePoolsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_node_pools"
}

func (d *nodePoolsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every live node pool of a cluster, in the API's order (by id). Pools that are still being deleted are included, with `status = \"deleting\"`.",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{Required: true, MarkdownDescription: "UUID of the cluster."},
			"node_pools": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The pools, with the same attributes as `data.aether_node_pool`.",
				NestedObject:        schema.NestedAttributeObject{Attributes: nodePoolDataAttributes(false)}},
		},
	}
}

func (d *nodePoolsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *nodePoolsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg nodePoolsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, ok := parseInputID(path.Root("cluster_id"), cfg.ClusterID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	all, err := listAllNodePools(ctx, d.pd, clusterID)
	if err != nil {
		apierr.ToDiagnostics(err, &resp.Diagnostics, path.Empty())
		return
	}
	out := nodePoolsModel{ClusterID: cfg.ClusterID, NodePools: make([]nodePoolDataModel, 0, len(all))}
	for i := range all {
		p, diags := nodePoolData(ctx, &all[i])
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		out.NodePools = append(out.NodePools, p)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
