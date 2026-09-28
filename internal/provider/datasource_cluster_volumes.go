package provider

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &clusterVolumesDataSource{}

// NewClusterVolumesDataSource returns data.aether_cluster_volumes.
func NewClusterVolumesDataSource() datasource.DataSource { return &clusterVolumesDataSource{} }

type clusterVolumesDataSource struct{ pd *providerData }

type volumeModel struct {
	ID               types.String  `tfsdk:"id"`
	Name             types.String  `tfsdk:"name"`
	Type             types.String  `tfsdk:"type"`
	StorageClass     types.String  `tfsdk:"storage_class"`
	ProvisionedBytes types.Int64   `tfsdk:"provisioned_bytes"`
	UsedBytes        types.Int64   `tfsdk:"used_bytes"`
	UsedPercent      types.Float64 `tfsdk:"used_percent"`
	BilledOn         types.String  `tfsdk:"billed_on"`
	State            types.String  `tfsdk:"state"`
	Watchers         types.Int64   `tfsdk:"watchers"`
	CreatedAt        types.String  `tfsdk:"created_at"`
}

type clusterVolumesModel struct {
	ClusterID           types.String  `tfsdk:"cluster_id"`
	Volumes             []volumeModel `tfsdk:"volumes"`
	InventoryAvailable  types.Bool    `tfsdk:"inventory_available"`
	LastSyncedAt        types.String  `tfsdk:"last_synced_at"`
	OrgSharedQuotaBytes types.Int64   `tfsdk:"org_shared_quota_bytes"`
	OrgSharedUsedBytes  types.Int64   `tfsdk:"org_shared_used_bytes"`
}

func (d *clusterVolumesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_volumes"
}

func (d *clusterVolumesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	num := func(desc string) schema.Int64Attribute {
		return schema.Int64Attribute{Computed: true, MarkdownDescription: desc}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "The persistent volumes of a cluster from the platform's storage inventory, sorted by `id`. " +
			"When `inventory_available` is false the platform could not read the storage inventory and `volumes` is empty — not a statement that the cluster has no volumes.",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{Required: true, MarkdownDescription: "UUID of the cluster."},
			"volumes": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The cluster's volumes, sorted by `id`.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":                str("CSI volume handle (block) or subvolume name (shared)."),
					"name":              str("Display name."),
					"type":              str("`block` or `shared`."),
					"storage_class":     str("`aether-block` or `aether-shared`."),
					"provisioned_bytes": num("Image size (block) or subvolume quota (shared); for a shared volume `0` means an unlimited quota, not an empty volume."),
					"used_bytes":        num("Bytes in use."),
					"used_percent":      schema.Float64Attribute{Computed: true, MarkdownDescription: "Used over provisioned, in percent; unclamped, and `0` when provisioned is `0`."},
					"billed_on":         str("Which figure the volume is billed on: `provisioned` or `used`."),
					"state":             str("State as reported by the platform."),
					"watchers":          num("Open block-device watchers (`0` = nothing has the image mapped)."),
					"created_at":        str("Creation time (RFC 3339)."),
				}}},
			"inventory_available":    schema.BoolAttribute{Computed: true, MarkdownDescription: "False when the platform could not read a current storage inventory; `volumes` is then empty."},
			"last_synced_at":         str("When the organization's inventory last completed a full sync (RFC 3339); the zero time when `inventory_available` is false."),
			"org_shared_quota_bytes": num("Quota of the organization-wide shared-storage pool, which every cluster of the organization shares; `0` means unlimited/unset."),
			"org_shared_used_bytes":  num("Bytes in use in that organization-wide pool (not the sum of this cluster's volumes)."),
		},
	}
}

func (d *clusterVolumesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterVolumesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg clusterVolumesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, ok := parseInputID(path.Root("cluster_id"), cfg.ClusterID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	res, err := d.pd.Client.ListClusterVolumesWithResponse(ctx, d.pd.orgID, clusterID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list the cluster's volumes", err.Error())
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
	body := res.JSON200
	out := clusterVolumesModel{
		ClusterID:           cfg.ClusterID,
		Volumes:             make([]volumeModel, 0, len(body.Volumes)),
		InventoryAvailable:  types.BoolValue(body.InventoryAvailable),
		LastSyncedAt:        types.StringValue(body.LastSyncedAt.Format(time.RFC3339)),
		OrgSharedQuotaBytes: types.Int64Value(body.OrgSharedQuotaBytes),
		OrgSharedUsedBytes:  types.Int64Value(body.OrgSharedUsedBytes),
	}
	for _, v := range body.Volumes {
		out.Volumes = append(out.Volumes, volumeModel{
			ID: types.StringValue(v.Id), Name: types.StringValue(v.Name), Type: types.StringValue(string(v.Type)),
			StorageClass: types.StringValue(string(v.StorageClass)), ProvisionedBytes: types.Int64Value(v.ProvisionedBytes),
			UsedBytes: types.Int64Value(v.UsedBytes), UsedPercent: types.Float64Value(v.UsedPercent),
			BilledOn: types.StringValue(string(v.BilledOn)), State: types.StringValue(v.State),
			Watchers: types.Int64Value(int64(v.Watchers)), CreatedAt: types.StringValue(v.CreatedAt.Format(time.RFC3339)),
		})
	}
	// The API serves the inventory's own order, which the platform does not
	// keep stable; sort so repeated reads do not reorder the list.
	sort.SliceStable(out.Volumes, func(i, j int) bool { return out.Volumes[i].ID.ValueString() < out.Volumes[j].ID.ValueString() })
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
