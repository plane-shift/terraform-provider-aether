package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSourceWithConfigure = &registryRobotAccountsDataSource{}

// NewRegistryRobotAccountsDataSource is aether_registry_robot_accounts (T14-6).
func NewRegistryRobotAccountsDataSource() datasource.DataSource {
	return &registryRobotAccountsDataSource{}
}

type registryRobotAccountsDataSource struct{ pd *providerData }

// robotAccountDataModel is the resource's attributes minus the secret (only
// the create answer carries it).
type robotAccountDataModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Permissions    types.Set    `tfsdk:"permissions"`
	HarborID       types.Int64  `tfsdk:"harbor_id"`
	HarborUsername types.String `tfsdk:"harbor_username"`
	CreatedAt      types.String `tfsdk:"created_at"`
	UpdatedAt      types.String `tfsdk:"updated_at"`
}

type registryRobotAccountsModel struct {
	RobotAccounts []robotAccountDataModel `tfsdk:"robot_accounts"`
}

func (d *registryRobotAccountsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry_robot_accounts"
}

func (d *registryRobotAccountsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every robot account of the organization's registry, including the default robot created when the registry was enabled. Secrets are never listed: the API returns a robot's secret only when it is created.",
		Attributes: map[string]schema.Attribute{
			"robot_accounts": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The robot accounts, in the API's order (by id).",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":              schema.StringAttribute{Computed: true, MarkdownDescription: "Robot account UUID."},
					"name":            schema.StringAttribute{Computed: true, MarkdownDescription: "Robot name."},
					"permissions":     schema.SetAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Subset of `push`, `pull`, `delete`."},
					"harbor_id":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Registry-side robot id; null when the platform holds none."},
					"harbor_username": schema.StringAttribute{Computed: true, MarkdownDescription: "The `docker login` username (`robot$<project>+<name>`); null when the platform holds none."},
					"created_at":      schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time (RFC 3339)."},
					"updated_at":      schema.StringAttribute{Computed: true, MarkdownDescription: "Last write time (RFC 3339)."},
				}}},
		},
	}
}

func (d *registryRobotAccountsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *registryRobotAccountsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	var all []robotAccountWire
	resp.Diagnostics.Append(pageRobotAccounts(ctx, d.pd, func(items []robotAccountWire) bool {
		all = append(all, items...)
		return false // read to the empty page
	})...)
	if resp.Diagnostics.HasError() {
		return
	}
	out := registryRobotAccountsModel{RobotAccounts: make([]robotAccountDataModel, 0, len(all))}
	for i := range all {
		var m robotAccountModel
		resp.Diagnostics.Append(flattenRobot(ctx, &all[i], &m)...)
		out.RobotAccounts = append(out.RobotAccounts, robotAccountDataModel{
			ID: m.ID, Name: m.Name, Permissions: m.Permissions, HarborID: m.HarborID,
			HarborUsername: m.HarborUsername, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
		})
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
