package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &nodePoolMachinesDataSource{}

// NewNodePoolMachinesDataSource returns data.aether_node_pool_machines.
func NewNodePoolMachinesDataSource() datasource.DataSource { return &nodePoolMachinesDataSource{} }

type nodePoolMachinesDataSource struct{ pd *providerData }

type machineModel struct {
	Name       types.String `tfsdk:"name"`
	Status     types.String `tfsdk:"status"`
	K8sVersion types.String `tfsdk:"k8s_version"`
	InternalIP types.String `tfsdk:"internal_ip"`
	AgeSeconds types.Int64  `tfsdk:"age_seconds"`
	CreatedAt  types.String `tfsdk:"created_at"`
}

type nodePoolMachinesModel struct {
	ClusterID  types.String   `tfsdk:"cluster_id"`
	NodePoolID types.String   `tfsdk:"node_pool_id"`
	Machines   []machineModel `tfsdk:"machines"`
}

func (d *nodePoolMachinesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_node_pool_machines"
}

func (d *nodePoolMachinesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The machines (nodes) of a node pool, sorted by name, as the platform currently sees them. " +
			"An empty list is also what the platform returns when it cannot read the cluster's machines; `age_seconds` changes on every read; `k8s_version` is the kubelet's own string (it may carry a leading `v`).",
		Attributes: map[string]schema.Attribute{
			"cluster_id":   schema.StringAttribute{Required: true, MarkdownDescription: "UUID of the cluster."},
			"node_pool_id": schema.StringAttribute{Required: true, MarkdownDescription: "UUID of the node pool."},
			"machines": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The pool's machines, sorted by name.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":        schema.StringAttribute{Computed: true, MarkdownDescription: "Machine name."},
					"status":      schema.StringAttribute{Computed: true, MarkdownDescription: "`Ready`, `NotReady`, `Provisioning` or `Deleting`."},
					"k8s_version": schema.StringAttribute{Computed: true, MarkdownDescription: "The kubelet's reported version, verbatim (may carry a leading `v`); `\"\"` until the node reports one."},
					"internal_ip": schema.StringAttribute{Computed: true, MarkdownDescription: "First internal IP address; `\"\"` until known."},
					"age_seconds": schema.Int64Attribute{Computed: true, MarkdownDescription: "Age at the time of the read; changes on every read."},
					"created_at":  schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time (RFC 3339); the zero time when unknown."},
				}}},
		},
	}
}

func (d *nodePoolMachinesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *nodePoolMachinesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg nodePoolMachinesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, ok := parseInputID(path.Root("cluster_id"), cfg.ClusterID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	poolID, ok := parseInputID(path.Root("node_pool_id"), cfg.NodePoolID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	res, err := d.pd.Client.ListNodePoolMachinesWithResponse(ctx, d.pd.orgID, clusterID, poolID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list the node pool's machines", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		// Includes the 502 kube_list_failed the API answers when it cannot
		// list the machines.
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	if res.JSON200 == nil {
		addNonJSONError(&resp.Diagnostics, res.HTTPResponse, res.Status())
		return
	}
	out := nodePoolMachinesModel{ClusterID: cfg.ClusterID, NodePoolID: cfg.NodePoolID, Machines: []machineModel{}}
	for _, m := range res.JSON200.Items {
		out.Machines = append(out.Machines, machineModel{
			Name: types.StringValue(m.Name), Status: types.StringValue(string(m.Status)),
			K8sVersion: types.StringValue(m.K8sVersion), InternalIP: types.StringValue(m.InternalIp),
			AgeSeconds: types.Int64Value(m.AgeSeconds), CreatedAt: types.StringValue(m.CreatedAt.Format(time.RFC3339)),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
