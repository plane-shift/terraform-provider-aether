package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var (
	_ datasource.DataSourceWithConfigure        = &clusterDataSource{}
	_ datasource.DataSourceWithConfigValidators = &clusterDataSource{}
)

// NewClusterDataSource returns data.aether_cluster.
func NewClusterDataSource() datasource.DataSource { return &clusterDataSource{} }

type clusterDataSource struct{ pd *providerData }

// clusterDataModel is one cluster as the data sources expose it: every
// aether_cluster attribute except timeouts, with k8s_version as the API's
// resolved text (there is no configured spelling to preserve).
type clusterDataModel struct {
	ID                        types.String `tfsdk:"id"`
	Name                      types.String `tfsdk:"name"`
	Region                    types.String `tfsdk:"region"`
	K8sVersion                types.String `tfsdk:"k8s_version"`
	PublicEndpoint            types.Bool   `tfsdk:"public_endpoint"`
	Plan                      types.String `tfsdk:"plan"`
	Status                    types.String `tfsdk:"status"`
	StatusMessage             types.String `tfsdk:"status_message"`
	Endpoint                  types.String `tfsdk:"endpoint"`
	ObservedKubernetesVersion types.String `tfsdk:"observed_kubernetes_version"`
	CapiClusterName           types.String `tfsdk:"capi_cluster_name"`
	KeycloakRealm             types.String `tfsdk:"keycloak_realm"`
	HarborProject             types.String `tfsdk:"harbor_project"`
	CephPool                  types.String `tfsdk:"ceph_pool"`
	LbSubnet                  types.String `tfsdk:"lb_subnet"`
	WorkerSubnet              types.String `tfsdk:"worker_subnet"`
	CreatedAt                 types.String `tfsdk:"created_at"`
	UpdatedAt                 types.String `tfsdk:"updated_at"`
}

// clusterData flattens an API cluster through the resource's flattenCluster,
// so the data sources and aether_cluster can never disagree on a field.
func clusterData(c *client.Cluster) clusterDataModel {
	var m clusterModel
	flattenCluster(c, &m)
	return clusterDataModel{
		ID: m.ID, Name: m.Name, Region: m.Region,
		K8sVersion:     types.StringValue(m.K8sVersion.ValueString()),
		PublicEndpoint: m.PublicEndpoint, Plan: m.Plan, Status: m.Status, StatusMessage: m.StatusMessage,
		Endpoint: m.Endpoint, ObservedKubernetesVersion: m.ObservedKubernetesVersion,
		CapiClusterName: m.CapiClusterName, KeycloakRealm: m.KeycloakRealm, HarborProject: m.HarborProject,
		CephPool: m.CephPool, LbSubnet: m.LbSubnet, WorkerSubnet: m.WorkerSubnet,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

// clusterDataAttributes is the attribute set of one cluster. lookup makes
// id and name the Optional inputs of data.aether_cluster; otherwise (an
// element of data.aether_clusters) everything is Computed.
func clusterDataAttributes(lookup bool) map[string]schema.Attribute {
	computed := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	attrs := map[string]schema.Attribute{
		"id":                          computed("Cluster UUID."),
		"name":                        computed("Cluster name (unique within the organization)."),
		"region":                      computed("Region id (e.g. `eu-fi`)."),
		"k8s_version":                 computed("The `MAJOR.MINOR.PATCH` the platform resolved the cluster's version to."),
		"public_endpoint":             schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the API server is exposed on a public IPv4 address and DNS name."},
		"plan":                        computed("Package slug; null when the cluster is billed per resource."),
		"status":                      computed("Lifecycle status reported by the API (`provisioning`, `running`, `upgrading`, `degraded`, `failed`, `deleting`, ...)."),
		"status_message":              computed("Human explanation accompanying the status, when the API offers one."),
		"endpoint":                    computed("API server URL, once the cluster is provisioned far enough to report one."),
		"observed_kubernetes_version": computed("Kubernetes version the control plane reports once a rollout settles."),
		"capi_cluster_name":           computed("Internal Cluster API name of the cluster."),
		"keycloak_realm":              computed("Identity realm of the cluster."),
		"harbor_project":              computed("Registry project associated with the cluster."),
		"ceph_pool":                   computed("Storage pool of the cluster."),
		"lb_subnet":                   computed("Load-balancer subnet allocated at create time."),
		"worker_subnet":               computed("Per-cluster worker /24 allocated at create time."),
		"created_at":                  computed("Creation time (RFC 3339)."),
		"updated_at":                  computed("Last update time (RFC 3339)."),
	}
	if lookup {
		attrs["id"] = schema.StringAttribute{Optional: true, Computed: true,
			MarkdownDescription: "Cluster UUID to look up. Exactly one of `id` and `name` must be set."}
		attrs["name"] = schema.StringAttribute{Optional: true, Computed: true,
			MarkdownDescription: "Cluster name to look up (names are unique within the organization). Exactly one of `id` and `name` must be set."}
	}
	return attrs
}

func (d *clusterDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func (d *clusterDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One cluster of the organization, looked up by `id` or by `name`. A cluster that does not exist is an error, never an empty result. " +
			"Clusters that are being deleted or have failed are still listed by the API and can be found by name until the platform removes them.",
		Attributes: clusterDataAttributes(true),
	}
}

func (d *clusterDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name")),
	}
}

func (d *clusterDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg clusterDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var found *client.Cluster
	if !cfg.ID.IsNull() {
		id, ok := parseInputID(path.Root("id"), cfg.ID.ValueString(), &resp.Diagnostics)
		if !ok {
			return
		}
		res, err := d.pd.Client.GetClusterWithResponse(ctx, d.pd.orgID, id)
		if err != nil {
			resp.Diagnostics.AddError("Cannot read the cluster", err.Error())
			return
		}
		if res.StatusCode() != http.StatusOK {
			// A 404 is an error here, never an empty result (spec §6).
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
		all, err := listAllClusters(ctx, d.pd)
		if err != nil {
			apierr.ToDiagnostics(err, &resp.Diagnostics, path.Empty())
			return
		}
		var matches []client.Cluster
		for _, c := range all {
			if c.Name == name {
				matches = append(matches, c)
			}
		}
		switch len(matches) {
		case 0:
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Cluster not found",
				fmt.Sprintf("There is no cluster named %q in organization %s.", name, d.pd.OrgID))
			return
		case 1:
			found = &matches[0]
		default:
			// Unreachable against the real API (UNIQUE(org_id, name)); kept
			// so a broken invariant fails loudly instead of picking one.
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Cluster name is ambiguous",
				fmt.Sprintf("The name %q matched %d clusters in organization %s; look the cluster up by id instead.", name, len(matches), d.pd.OrgID))
			return
		}
	}
	out := clusterData(found)
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

// parseInputID parses a configured id attribute. It must be canonical
// lower-case UUID text: Terraform rejects a data source that returns a
// configured value in a different spelling, and the API answers with the
// canonical form, so anything else is refused here with a clear message.
func parseInputID(p path.Path, s string, diags *diag.Diagnostics) (client.ClusterID, bool) {
	id, err := parseID(s)
	if err != nil || id.String() != s {
		diags.AddAttributeError(p, "Invalid id",
			fmt.Sprintf("%q must be a canonical lower-case UUID (36 characters, lower-case hex and hyphens).", s))
		return id, false
	}
	return id, true
}

// listPageLimit is the page size the list data sources request: the API's
// maximum (1..100; anything else silently falls back to 50).
const listPageLimit = 100

// listAllClusters reads every cluster of the organization, page by page.
// The API sets next_cursor only on a FULL page, so a total that is a
// multiple of the limit costs one trailing empty page.
func listAllClusters(ctx context.Context, pd *providerData) ([]client.Cluster, error) {
	return collectAll(ctx, func(ctx context.Context, cursor string) ([]client.Cluster, string, error) {
		limit := listPageLimit
		params := &client.ListClustersParams{Limit: &limit}
		if cursor != "" {
			c, err := parseCursor(cursor)
			if err != nil {
				return nil, "", err
			}
			params.Cursor = &c
		}
		res, err := pd.Client.ListClustersWithResponse(ctx, pd.orgID, params)
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

// parseCursor turns a next_cursor from the API into the UUID the list
// parameters carry. A cursor that is not a UUID is an error: sent back as
// anything else, the API would silently restart from page 1 and the walk
// would never end.
func parseCursor(s string) (client.Cursor, error) {
	c, err := parseID(s)
	if err != nil {
		return c, fmt.Errorf("the API returned a next_cursor that is not a UUID: %w", err)
	}
	return c, nil
}
