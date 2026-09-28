package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &organizationDataSource{}

// NewOrganizationDataSource returns data.aether_organization.
func NewOrganizationDataSource() datasource.DataSource { return &organizationDataSource{} }

type organizationDataSource struct{ pd *providerData }

// nonJSONSummary is the summary of the diagnostic for a 200 whose body the
// generated client could not decode: it is not a reading of anything, so it
// is an error, never an empty result (T13-14).
const nonJSONSummary = "Aether API answered 200 without a JSON body"

// addNonJSONError adds the nonJSONSummary diagnostic for hr.
func addNonJSONError(diags *diag.Diagnostics, hr *http.Response, status string) {
	diags.AddError(nonJSONSummary, describeNonJSON(hr, status)+".")
}

func (d *organizationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization"
}

func (d *organizationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The provider's organization (the one `organization_id` names, or the one the API token belongs to). `billing_email` and `vat_id` are null when the organization has never set them.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "Organization UUID."},
			"name":          schema.StringAttribute{Computed: true, MarkdownDescription: "Display name."},
			"slug":          schema.StringAttribute{Computed: true, MarkdownDescription: "URL-safe slug (also names the organization's registry project)."},
			"billing_email": schema.StringAttribute{Computed: true, MarkdownDescription: "Billing contact address; null when unset."},
			"vat_id":        schema.StringAttribute{Computed: true, MarkdownDescription: "VAT identifier; null when unset."},
			"plan":          schema.StringAttribute{Computed: true, MarkdownDescription: "Organization plan."},
			"created_at":    schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time (RFC 3339)."},
			"updated_at":    schema.StringAttribute{Computed: true, MarkdownDescription: "Last update time (RFC 3339)."},
		},
	}
}

func (d *organizationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *organizationDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	res, err := d.pd.Client.GetOrganizationWithResponse(ctx, d.pd.orgID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the organization", err.Error())
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
	var out organizationModel
	flattenOrganization(res.JSON200, &out)
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
