package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var (
	_ datasource.DataSourceWithConfigure        = &nodePoolDataSource{}
	_ datasource.DataSourceWithConfigValidators = &nodePoolDataSource{}
)

// NewNodePoolDataSource returns data.aether_node_pool.
func NewNodePoolDataSource() datasource.DataSource { return &nodePoolDataSource{} }

type nodePoolDataSource struct{ pd *providerData }

// nodePoolDataModel is one node pool as the data sources expose it: every
// aether_node_pool attribute except timeouts.
type nodePoolDataModel struct {
	ID                          types.String `tfsdk:"id"`
	ClusterID                   types.String `tfsdk:"cluster_id"`
	Name                        types.String `tfsdk:"name"`
	Vcpus                       types.Int64  `tfsdk:"vcpus"`
	MemoryGb                    types.Int64  `tfsdk:"memory_gb"`
	DiskGb                      types.Int64  `tfsdk:"disk_gb"`
	DesiredNodes                types.Int64  `tfsdk:"desired_nodes"`
	MinNodes                    types.Int64  `tfsdk:"min_nodes"`
	MaxNodes                    types.Int64  `tfsdk:"max_nodes"`
	MinVcpus                    types.Int64  `tfsdk:"min_vcpus"`
	MaxVcpus                    types.Int64  `tfsdk:"max_vcpus"`
	MinMemoryGb                 types.Int64  `tfsdk:"min_memory_gb"`
	MaxMemoryGb                 types.Int64  `tfsdk:"max_memory_gb"`
	NodeLabels                  types.Map    `tfsdk:"node_labels"`
	NodeTaints                  types.List   `tfsdk:"node_taints"`
	DrainTimeoutSeconds         types.Int64  `tfsdk:"drain_timeout_seconds"`
	ScaleDownDelaySeconds       types.Int64  `tfsdk:"scale_down_delay_seconds"`
	ScaleDownUtilizationPercent types.Int64  `tfsdk:"scale_down_utilization_percent"`
	CurrentNodes                types.Int64  `tfsdk:"current_nodes"`
	Status                      types.String `tfsdk:"status"`
	StatusMessage               types.String `tfsdk:"status_message"`
	FirstReadyAt                types.String `tfsdk:"first_ready_at"`
	CreatedAt                   types.String `tfsdk:"created_at"`
	UpdatedAt                   types.String `tfsdk:"updated_at"`
}

// nodePoolData flattens an API pool through the resource's flattenNodePool.
// Labels and taints start as EMPTY (not null) values, so a pool without any
// reads as an empty map/list: a data source has no configuration whose null
// spelling should be preserved.
func nodePoolData(ctx context.Context, p *client.NodePool) (nodePoolDataModel, diag.Diagnostics) {
	m := nodePoolModel{
		NodeLabels: types.MapValueMust(types.StringType, map[string]attr.Value{}),
		NodeTaints: types.ListValueMust(nodeTaintType, []attr.Value{}),
	}
	diags := flattenNodePool(ctx, p, &m)
	return nodePoolDataModel{
		ID: m.ID, ClusterID: m.ClusterID, Name: m.Name,
		Vcpus: m.Vcpus, MemoryGb: m.MemoryGb, DiskGb: m.DiskGb,
		DesiredNodes: m.DesiredNodes, MinNodes: m.MinNodes, MaxNodes: m.MaxNodes,
		MinVcpus: m.MinVcpus, MaxVcpus: m.MaxVcpus, MinMemoryGb: m.MinMemoryGb, MaxMemoryGb: m.MaxMemoryGb,
		NodeLabels: m.NodeLabels, NodeTaints: m.NodeTaints,
		DrainTimeoutSeconds: m.DrainTimeoutSeconds, ScaleDownDelaySeconds: m.ScaleDownDelaySeconds,
		ScaleDownUtilizationPercent: m.ScaleDownUtilizationPercent,
		CurrentNodes:                m.CurrentNodes, Status: m.Status, StatusMessage: m.StatusMessage,
		FirstReadyAt: m.FirstReadyAt, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}, diags
}

// nodePoolDataAttributes is the attribute set of one node pool. lookup makes
// cluster_id Required and id/name the Optional inputs of
// data.aether_node_pool; otherwise everything is Computed.
func nodePoolDataAttributes(lookup bool) map[string]schema.Attribute {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	num := func(desc string) schema.Int64Attribute {
		return schema.Int64Attribute{Computed: true, MarkdownDescription: desc}
	}
	attrs := map[string]schema.Attribute{
		"id":            str("Node pool UUID."),
		"cluster_id":    str("UUID of the cluster the pool belongs to."),
		"name":          str("Pool name (unique among the cluster's live pools)."),
		"vcpus":         num("vCPUs per node."),
		"memory_gb":     num("Memory per node in GB."),
		"disk_gb":       num("Disk per node in GB."),
		"desired_nodes": num("Requested node count of a fixed pool."),
		"min_nodes":     num("Stored lower node bound. Autoscaling is decided by the four budget attributes, not by this value."),
		"max_nodes":     num("Stored upper node bound. Autoscaling is decided by the four budget attributes, not by this value."),
		"min_vcpus":     num("Autoscaling budget: minimum total vCPUs; `0` means no budget."),
		"max_vcpus":     num("Autoscaling budget: maximum total vCPUs; `0` means no budget."),
		"min_memory_gb": num("Autoscaling budget: minimum total memory in GB; `0` means no budget."),
		"max_memory_gb": num("Autoscaling budget: maximum total memory in GB; `0` means no budget."),
		"node_labels": schema.MapAttribute{Computed: true, ElementType: types.StringType,
			MarkdownDescription: "Kubernetes labels applied to every node; empty when none."},
		"node_taints": schema.ListNestedAttribute{Computed: true,
			MarkdownDescription: "Kubernetes taints applied to every node; empty when none.",
			NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
				"key":    str("Taint key."),
				"value":  str("Taint value (`\"\"` when none)."),
				"effect": str("`NoSchedule`, `PreferNoSchedule` or `NoExecute`."),
			}}},
		"drain_timeout_seconds":          num("Per-node drain timeout in seconds; null when the pool tracks the platform default."),
		"scale_down_delay_seconds":       num("Autoscaling: how long a node must be unneeded before removal; null for the platform default."),
		"scale_down_utilization_percent": num("Autoscaling: utilization below which a node counts as unneeded; null for the platform default."),
		"current_nodes":                  num("Nodes the platform currently observes."),
		"status":                         str("Lifecycle status: `provisioning`, `running`, `scaling`, `scaling-down`, `degraded` or `deleting`."),
		"status_message":                 str("Explanation accompanying a `degraded` status."),
		"first_ready_at":                 str("Time the pool first had a ready node (RFC 3339); null until then."),
		"created_at":                     str("Creation time (RFC 3339)."),
		"updated_at":                     str("Last update time (RFC 3339)."),
	}
	if lookup {
		attrs["cluster_id"] = schema.StringAttribute{Required: true,
			MarkdownDescription: "UUID of the cluster the pool belongs to."}
		attrs["id"] = schema.StringAttribute{Optional: true, Computed: true,
			MarkdownDescription: "Node pool UUID to look up. Exactly one of `id` and `name` must be set."}
		attrs["name"] = schema.StringAttribute{Optional: true, Computed: true,
			MarkdownDescription: "Pool name to look up. Exactly one of `id` and `name` must be set. A pool that is still being deleted (`status = \"deleting\"`) holds its name until the platform removes it, so it can match."}
	}
	return attrs
}

func (d *nodePoolDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_node_pool"
}

func (d *nodePoolDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One node pool of a cluster, looked up by `id` or by `name`. A pool that does not exist is an error, never an empty result. " +
			"A pool that is still being deleted can match by name (its `status` is then `deleting`).",
		Attributes: nodePoolDataAttributes(true),
	}
}

func (d *nodePoolDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name")),
	}
}

func (d *nodePoolDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *nodePoolDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg nodePoolDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, ok := parseInputID(path.Root("cluster_id"), cfg.ClusterID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	var found *client.NodePool
	if !cfg.ID.IsNull() {
		poolID, ok := parseInputID(path.Root("id"), cfg.ID.ValueString(), &resp.Diagnostics)
		if !ok {
			return
		}
		res, err := d.pd.Client.GetNodePoolWithResponse(ctx, d.pd.orgID, clusterID, poolID)
		if err != nil {
			resp.Diagnostics.AddError("Cannot read the node pool", err.Error())
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
		found = res.JSON200
	} else {
		name := cfg.Name.ValueString()
		all, err := listAllNodePools(ctx, d.pd, clusterID)
		if err != nil {
			apierr.ToDiagnostics(err, &resp.Diagnostics, path.Empty())
			return
		}
		var matches []client.NodePool
		for _, p := range all {
			if p.Name == name {
				matches = append(matches, p)
			}
		}
		switch len(matches) {
		case 0:
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Node pool not found",
				fmt.Sprintf("There is no node pool named %q in cluster %s.", name, clusterID))
			return
		case 1:
			found = &matches[0]
		default:
			// Unreachable against the real API (names are unique among live
			// pools); kept so a broken invariant fails loudly.
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Node pool name is ambiguous",
				fmt.Sprintf("The name %q matched %d node pools in cluster %s; look the pool up by id instead.", name, len(matches), clusterID))
			return
		}
	}
	out, diags := nodePoolData(ctx, found)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

// listAllNodePools reads every live node pool of a cluster, page by page
// (same paging contract as listAllClusters).
func listAllNodePools(ctx context.Context, pd *providerData, clusterID client.ClusterID) ([]client.NodePool, error) {
	return collectAll(ctx, func(ctx context.Context, cursor string) ([]client.NodePool, string, error) {
		limit := listPageLimit
		params := &client.ListNodePoolsParams{Limit: &limit}
		if cursor != "" {
			c, err := parseCursor(cursor)
			if err != nil {
				return nil, "", err
			}
			params.Cursor = &c
		}
		res, err := pd.Client.ListNodePoolsWithResponse(ctx, pd.orgID, clusterID, params)
		if err != nil {
			return nil, "", err
		}
		if res.StatusCode() != http.StatusOK {
			return nil, "", apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body)
		}
		if res.JSON200 == nil {
			return nil, "", errors.New(describeNonJSON(res.HTTPResponse, res.Status()))
		}
		return res.JSON200.Items, deref(res.JSON200.NextCursor), nil
	})
}
