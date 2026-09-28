package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &regionsDataSource{}

func NewRegionsDataSource() datasource.DataSource { return &regionsDataSource{} }

type regionsDataSource struct{ pd *providerData }

type regionModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Location  types.String `tfsdk:"location"`
	Available types.Bool   `tfsdk:"available"`
}

type regionsModel struct {
	Regions []regionModel `tfsdk:"regions"`
}

func (d *regionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_regions"
}

func (d *regionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Regions the platform knows about. `available` is true only for the region this API instance serves.",
		Attributes: map[string]schema.Attribute{
			"regions": schema.ListNestedAttribute{Computed: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
				"id": schema.StringAttribute{Computed: true}, "name": schema.StringAttribute{Computed: true},
				"location": schema.StringAttribute{Computed: true}, "available": schema.BoolAttribute{Computed: true},
			}}},
		},
	}
}

func (d *regionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *regionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	res, err := d.pd.Client.ListRegionsWithResponse(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list regions", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	if res.JSON200 == nil {
		resp.Diagnostics.AddError("Aether API answered 200 without a JSON body",
			fmt.Sprintf("GET %s returned %s with content type %q.",
				res.HTTPResponse.Request.URL, res.Status(), res.HTTPResponse.Header.Get("Content-Type")))
		return
	}
	out := regionsModel{Regions: []regionModel{}}
	for _, r := range res.JSON200.Items {
		out.Regions = append(out.Regions, regionModel{ID: types.StringValue(r.Id), Name: types.StringValue(r.Name), Location: types.StringValue(r.Location), Available: types.BoolValue(r.Available)})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
