package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &organizationMembersDataSource{}

// NewOrganizationMembersDataSource is aether_organization_members (T14-1).
func NewOrganizationMembersDataSource() datasource.DataSource {
	return &organizationMembersDataSource{}
}

type organizationMembersDataSource struct{ pd *providerData }

type organizationMembersModel struct {
	// The member attributes are exactly the aether_organization_member
	// resource's, so its model and flattenMember are reused.
	Members []organizationMemberModel `tfsdk:"members"`
}

// memberListDSPageSize is the page size the data source asks for (the API
// maximum, T14-1).
const memberListDSPageSize = 100

func (d *organizationMembersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_organization_members"
}

func (d *organizationMembersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every membership of the provider's organization.",
		Attributes: map[string]schema.Attribute{
			"members": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The memberships, in the API's order (by membership id).",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Membership UUID (not a user id)."},
					"email":       schema.StringAttribute{Computed: true, MarkdownDescription: "Login email address of the member."},
					"role":        schema.StringAttribute{Computed: true, MarkdownDescription: "`owner`, `admin`, `member` or `viewer`."},
					"invited_by":  schema.StringAttribute{Computed: true, MarkdownDescription: "UUID of the user who invited the member; null when not recorded (always for memberships created through the API)."},
					"accepted_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Time the invitation was accepted (RFC 3339); null when not recorded."},
					"created_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time of the membership (RFC 3339)."},
				}}},
		},
	}
}

func (d *organizationMembersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *organizationMembersDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	members, err := collectAll(ctx, func(ctx context.Context, cursor string) ([]client.Member, string, error) {
		c, err := uuidCursorParam(cursor)
		if err != nil {
			return nil, "", err
		}
		limit := memberListDSPageSize
		res, err := d.pd.Client.ListMembersWithResponse(ctx, d.pd.orgID, &client.ListMembersParams{Limit: &limit, Cursor: c})
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
			return nil, "", fmt.Errorf("member list: %w", err)
		}
		return res.JSON200.Items, next, nil
	})
	if err != nil {
		addPageError(&resp.Diagnostics, "Cannot list organization members", err)
		return
	}
	out := organizationMembersModel{Members: make([]organizationMemberModel, 0, len(members))}
	for i := range members {
		var m organizationMemberModel
		flattenMember(&members[i], &m)
		out.Members = append(out.Members, m)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
