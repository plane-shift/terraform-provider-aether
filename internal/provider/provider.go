package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/transport"
)

const defaultEndpoint = "https://api.aetherplatform.cloud"

// providerData is what every resource and data source receives from Configure.
type providerData struct {
	Client *client.ClientWithResponses
	OrgID  string
}

type aetherProvider struct{ version string }

type providerModel struct {
	APIToken       types.String `tfsdk:"api_token"`
	Endpoint       types.String `tfsdk:"endpoint"`
	OrganizationID types.String `tfsdk:"organization_id"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &aetherProvider{version: version} }
}

func (p *aetherProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "aether"
	resp.Version = p.version
}

func (p *aetherProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage Aether Kubernetes clusters, access and registry. Create an API token in the Aether portal first; the token belongs to one organization, which the provider uses for every resource.",
		Attributes: map[string]schema.Attribute{
			"api_token": schema.StringAttribute{
				Optional: true, Sensitive: true,
				MarkdownDescription: "API token created in the portal (`aether_…`). Falls back to `AETHER_API_TOKEN`.",
			},
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "API base URL. Defaults to `" + defaultEndpoint + "`; falls back to `AETHER_ENDPOINT`.",
			},
			"organization_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Organization the token belongs to. Optional: resolved automatically when the token sees exactly one organization. Falls back to `AETHER_ORGANIZATION_ID`.",
			},
		},
	}
}

func (p *aetherProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	token := firstNonEmpty(cfg.APIToken.ValueString(), os.Getenv("AETHER_API_TOKEN"))
	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv("AETHER_ENDPOINT"), defaultEndpoint)
	orgID := firstNonEmpty(cfg.OrganizationID.ValueString(), os.Getenv("AETHER_ORGANIZATION_ID"))
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("api_token"), "Missing API token",
			"Set api_token in the provider block or the AETHER_API_TOKEN environment variable. Tokens are created in the Aether portal.")
		return
	}

	httpClient := &http.Client{Transport: transport.New(token, http.DefaultTransport)}
	c, err := client.NewClientWithResponses(endpoint, client.WithHTTPClient(httpClient))
	if err != nil {
		resp.Diagnostics.AddError("Cannot build API client", err.Error())
		return
	}

	if orgID != "" {
		// Validate the override: the token belongs to exactly one organization.
		g, err := c.GetOrganizationWithResponse(ctx, orgID)
		if err != nil {
			resp.Diagnostics.AddError("Cannot reach the Aether API", fmt.Sprintf("GET %s/api/v1/organizations/%s: %s", endpoint, orgID, err))
			return
		}
		if g.StatusCode() != http.StatusOK {
			resp.Diagnostics.AddAttributeError(path.Root("organization_id"), "organization_id is not visible to this token",
				fmt.Sprintf("The API answered %s for organization %s. The token belongs to one organization; omit organization_id to use it.", g.Status(), orgID))
			return
		}
	} else {
		list, err := c.ListOrganizationsWithResponse(ctx, nil)
		if err != nil {
			resp.Diagnostics.AddError("Cannot reach the Aether API", fmt.Sprintf("GET %s/api/v1/organizations: %s", endpoint, err))
			return
		}
		switch list.StatusCode() {
		case http.StatusOK:
		case http.StatusUnauthorized, http.StatusForbidden:
			resp.Diagnostics.AddError("Aether API rejected the token",
				"The API answered "+list.Status()+". Check api_token (created in the portal) and, if set, organization_id.")
			return
		default:
			resp.Diagnostics.AddError("Cannot list organizations", list.Status()+": "+string(list.Body))
			return
		}
		items := list.JSON200.Items
		if len(items) != 1 {
			resp.Diagnostics.AddAttributeError(path.Root("organization_id"), "Cannot resolve the organization",
				fmt.Sprintf("The token sees %d organizations; set organization_id explicitly.", len(items)))
			return
		}
		orgID = items[0].Id
	}

	data := &providerData{Client: c, OrgID: orgID}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *aetherProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{} // filled task by task
}

func (p *aetherProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{} // filled task by task
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// fromProviderData is the shared Configure body of every resource/data source.
func fromProviderData(raw any, diags interface{ AddError(string, string) }) *providerData {
	if raw == nil {
		return nil
	}
	pd, ok := raw.(*providerData)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("got %T", raw))
		return nil
	}
	return pd
}
