package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSourceWithConfigure = &registryCleanupPoliciesDataSource{}

// NewRegistryCleanupPoliciesDataSource is aether_registry_cleanup_policies (T14-7).
func NewRegistryCleanupPoliciesDataSource() datasource.DataSource {
	return &registryCleanupPoliciesDataSource{}
}

type registryCleanupPoliciesDataSource struct{ pd *providerData }

type registryCleanupPoliciesModel struct {
	// The policy attributes are exactly the aether_registry_cleanup_policy
	// resource's, so its model and flatten are reused (same normalisation).
	Policies []cleanupPolicyModel `tfsdk:"policies"`
}

func (d *registryCleanupPoliciesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_registry_cleanup_policies"
}

func (d *registryCleanupPoliciesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every cleanup policy of the organization's registry. Policies survive disabling the registry.",
		Attributes: map[string]schema.Attribute{
			"policies": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The cleanup policies, in the API's order.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Cleanup policy UUID."},
					"name":       schema.StringAttribute{Computed: true, MarkdownDescription: "Policy name."},
					"action":     schema.StringAttribute{Computed: true, MarkdownDescription: "`delete` or `protect`."},
					"schedule":   schema.StringAttribute{Computed: true, MarkdownDescription: "`nightly`, `weekly` or `manual`."},
					"repo_scope": schema.SetAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Repository globs; empty means all repositories."},
					"enabled":    schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the scheduler runs the policy."},
					"conditions": schema.ListNestedAttribute{Computed: true,
						MarkdownDescription: "Conditions an artifact must ALL satisfy. `value` is always rendered as a string (a stored number or boolean is converted); `op`/`value` are null when the policy has none.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"type":  schema.StringAttribute{Computed: true, MarkdownDescription: "`age`, `tag_count`, `tag_name` or `untagged`."},
							"op":    schema.StringAttribute{Computed: true, MarkdownDescription: "Comparison operator; null when absent."},
							"value": schema.StringAttribute{Computed: true, MarkdownDescription: "Operand as a string; null when absent."},
						}}},
					"last_run_at":          schema.StringAttribute{Computed: true, MarkdownDescription: "Time of the last run (RFC 3339); null until the policy has run."},
					"last_run_deleted":     schema.Int64Attribute{Computed: true, MarkdownDescription: "Artifacts deleted by the last run; null until the policy has run."},
					"last_run_freed_bytes": schema.Int64Attribute{Computed: true, MarkdownDescription: "Bytes freed by the last run; null until the policy has run."},
					"created_at":           schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time (RFC 3339)."},
					"updated_at":           schema.StringAttribute{Computed: true, MarkdownDescription: "Time the policy was last written (RFC 3339); runs move it too."},
				}}},
		},
	}
}

func (d *registryCleanupPoliciesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *registryCleanupPoliciesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	items, diags := listCleanupPolicies(ctx, d.pd)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	out := registryCleanupPoliciesModel{Policies: make([]cleanupPolicyModel, 0, len(items))}
	for i := range items {
		var m cleanupPolicyModel
		resp.Diagnostics.Append(flattenCleanupPolicy(ctx, &items[i], &m)...)
		flattenCleanupLastRun(&items[i], &m)
		out.Policies = append(out.Policies, m)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
