package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &k8sVersionsDataSource{}

// NewK8sVersionsDataSource returns data.aether_k8s_versions.
func NewK8sVersionsDataSource() datasource.DataSource { return &k8sVersionsDataSource{} }

type k8sVersionsDataSource struct{ pd *providerData }

type k8sVersionModel struct {
	Version types.String `tfsdk:"version"`
	Label   types.String `tfsdk:"label"`
	Patch   types.String `tfsdk:"patch"`
}

type k8sVersionsModel struct {
	Versions []k8sVersionModel `tfsdk:"versions"`
	Latest   types.String      `tfsdk:"latest"`
}

func (d *k8sVersionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_k8s_versions"
}

func (d *k8sVersionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Kubernetes versions the platform currently offers for new clusters and upgrades, newest first. " +
			"An empty list means the platform's version window is unavailable right now, not that no versions exist; `latest` is then null.",
		Attributes: map[string]schema.Attribute{
			"versions": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "Offered minors in the API's order (newest first).",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"version": schema.StringAttribute{Computed: true, MarkdownDescription: "`MAJOR.MINOR`, the value `aether_cluster.k8s_version` accepts."},
					"label":   schema.StringAttribute{Computed: true, MarkdownDescription: "`Latest` on the newest offered minor, `Stable` on the others."},
					"patch":   schema.StringAttribute{Computed: true, MarkdownDescription: "The `MAJOR.MINOR.PATCH` the platform resolves this minor to."},
				}}},
			"latest": schema.StringAttribute{Computed: true,
				MarkdownDescription: "The `patch` of the version labelled `Latest`; null when the list is empty."},
		},
	}
}

func (d *k8sVersionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *k8sVersionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	res, err := d.pd.Client.ListK8sVersionsWithResponse(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list Kubernetes versions", err.Error())
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
	out := k8sVersionsModel{Versions: []k8sVersionModel{}, Latest: types.StringNull()}
	for _, v := range res.JSON200.Items {
		out.Versions = append(out.Versions, k8sVersionModel{
			Version: types.StringValue(v.Version), Label: types.StringValue(string(v.Label)), Patch: types.StringValue(v.Patch),
		})
		// The API labels exactly the first item Latest; take the first
		// labelled one, and leave latest null if none is (an empty window).
		if v.Label == client.Latest && out.Latest.IsNull() {
			out.Latest = types.StringValue(v.Patch)
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
