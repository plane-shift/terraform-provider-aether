package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &webhooksDataSource{}

// NewWebhooksDataSource is aether_webhooks (T14-2).
func NewWebhooksDataSource() datasource.DataSource { return &webhooksDataSource{} }

type webhooksDataSource struct{ pd *providerData }

// webhookDataModel is one webhook of the list: the resource's attributes
// minus the secret (the list never returns it) and the rotation trigger.
type webhookDataModel struct {
	ID          types.String `tfsdk:"id"`
	URL         types.String `tfsdk:"url"`
	Description types.String `tfsdk:"description"`
	Events      types.Set    `tfsdk:"events"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	CreatedAt   types.String `tfsdk:"created_at"`
	UpdatedAt   types.String `tfsdk:"updated_at"`
}

type webhooksModel struct {
	Webhooks []webhookDataModel `tfsdk:"webhooks"`
}

const webhookListDSPageSize = 100

func (d *webhooksDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_webhooks"
}

func (d *webhooksDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every webhook of the provider's organization. Signing secrets are never listed: the API returns a secret only when a webhook is created or its secret rotated.",
		Attributes: map[string]schema.Attribute{
			"webhooks": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The webhooks, in the API's order.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Webhook UUID."},
					"url":         schema.StringAttribute{Computed: true, MarkdownDescription: "Delivery URL."},
					"description": schema.StringAttribute{Computed: true, MarkdownDescription: "Description (`\"\"` when none)."},
					"events":      schema.SetAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Subscribed event types (`*` = all)."},
					"enabled":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether deliveries are sent."},
					"created_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time (RFC 3339)."},
					"updated_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Last write time (RFC 3339); a secret rotation moves it too."},
				}}},
		},
	}
}

func (d *webhooksDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *webhooksDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	hooks, err := collectAll(ctx, func(ctx context.Context, cursor string) ([]client.Webhook, string, error) {
		c, err := uuidCursorParam(cursor)
		if err != nil {
			return nil, "", err
		}
		limit := webhookListDSPageSize
		res, err := d.pd.Client.ListWebhooksWithResponse(ctx, d.pd.orgID, &client.ListWebhooksParams{Limit: &limit, Cursor: c})
		if err != nil {
			return nil, "", err
		}
		var diags diag.Diagnostics
		if res.StatusCode() != http.StatusOK {
			apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &diags, path.Empty())
			return nil, "", pageFailure(diags)
		}
		if res.JSON200 == nil {
			addNoJSONBodyError(&diags, res.HTTPResponse, res.Status())
			return nil, "", pageFailure(diags)
		}
		next, err := nextUUIDCursor(res.JSON200.NextCursor)
		if err != nil {
			return nil, "", fmt.Errorf("webhook list: %w", err)
		}
		return res.JSON200.Items, next, nil
	})
	if err != nil {
		addPageError(&resp.Diagnostics, "Cannot list webhooks", err)
		return
	}
	out := webhooksModel{Webhooks: make([]webhookDataModel, 0, len(hooks))}
	for i := range hooks {
		var m webhookModel
		resp.Diagnostics.Append(flattenWebhook(ctx, &hooks[i], &m)...)
		out.Webhooks = append(out.Webhooks, webhookDataModel{
			ID: m.ID, URL: m.URL, Description: m.Description, Events: m.Events,
			Enabled: m.Enabled, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
