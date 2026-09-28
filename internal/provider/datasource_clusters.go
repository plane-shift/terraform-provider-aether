package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &clustersDataSource{}

// NewClustersDataSource returns data.aether_clusters.
func NewClustersDataSource() datasource.DataSource { return &clustersDataSource{} }

type clustersDataSource struct{ pd *providerData }

type clustersModel struct {
	Clusters []clusterDataModel `tfsdk:"clusters"`
}

func (d *clustersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clusters"
}

func (d *clustersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every cluster of the organization, in the API's order (by id). Includes clusters that are being deleted or have failed, until the platform removes them.",
		Attributes: map[string]schema.Attribute{
			"clusters": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The clusters, with the same attributes as `data.aether_cluster`.",
				NestedObject:        schema.NestedAttributeObject{Attributes: clusterDataAttributes(false)}},
		},
	}
}

func (d *clustersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *clustersDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	all, err := listAllClusters(ctx, d.pd)
	if err != nil {
		apierr.ToDiagnostics(err, &resp.Diagnostics, path.Empty())
		return
	}
	out := clustersModel{Clusters: make([]clusterDataModel, 0, len(all))}
	for i := range all {
		out.Clusters = append(out.Clusters, clusterData(&all[i]))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
