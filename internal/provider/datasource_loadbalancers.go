package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &loadBalancersDataSource{}

// NewLoadBalancersDataSource returns data.aether_loadbalancers.
func NewLoadBalancersDataSource() datasource.DataSource { return &loadBalancersDataSource{} }

type loadBalancersDataSource struct{ pd *providerData }

type loadBalancerModel struct {
	IP          types.String `tfsdk:"ip"`
	Purpose     types.String `tfsdk:"purpose"`
	ServiceRef  types.String `tfsdk:"service_ref"`
	AllocatedAt types.String `tfsdk:"allocated_at"`
}

type loadBalancersModel struct {
	ClusterID     types.String        `tfsdk:"cluster_id"`
	LoadBalancers []loadBalancerModel `tfsdk:"loadbalancers"`
}

// loadBalancerWire is one item as the API actually sends it. The vendored
// OpenAPI (and so the generated LoadBalancer type) still describes a stale
// {service_name, namespace, external_ip, dns_name, ports} shape, into which
// every real item decodes as zero values - so the body is decoded here
// (aether-api issue #315).
type loadBalancerWire struct {
	IP          string     `json:"ip"`
	Purpose     string     `json:"purpose"`
	ServiceRef  *string    `json:"service_ref"`
	AllocatedAt *time.Time `json:"allocated_at"`
}

func (d *loadBalancersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_loadbalancers"
}

func (d *loadBalancersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Public addresses allocated to the cluster: `cp_lb` is the cluster's own API endpoint, `ccm_lb` a tenant LoadBalancer Service (`service_ref` = its UID); the DNS name is not exposed by the API today (aether-api issue).",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{Required: true, Validators: []validator.String{canonicalIDValidator{}}, MarkdownDescription: "UUID of the cluster."},
			"loadbalancers": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The addresses, in the API's order (by allocation time, then address).",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"ip":           schema.StringAttribute{Computed: true, MarkdownDescription: "Public IPv4 address."},
					"purpose":      schema.StringAttribute{Computed: true, MarkdownDescription: "`cp_lb` (the cluster's API endpoint) or `ccm_lb` (a tenant LoadBalancer Service)."},
					"service_ref":  schema.StringAttribute{Computed: true, MarkdownDescription: "UID of the tenant Service a `ccm_lb` address belongs to; null otherwise."},
					"allocated_at": schema.StringAttribute{Computed: true, MarkdownDescription: "Allocation time (RFC 3339); null when the platform did not record one."},
				}}},
		},
	}
}

func (d *loadBalancersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *loadBalancersDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg loadBalancersModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, ok := parseInputID(path.Root("cluster_id"), cfg.ClusterID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	res, err := d.pd.Client.ListLoadBalancersWithResponse(ctx, d.pd.orgID, clusterID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list the cluster's load balancers", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	var wire struct {
		Items *[]loadBalancerWire `json:"items"`
	}
	if err := json.Unmarshal(res.Body, &wire); err != nil || wire.Items == nil {
		var detail string
		if err != nil {
			detail = describeNonJSON(res.HTTPResponse, res.Status()) + ": " + err.Error()
		} else {
			detail = fmt.Sprintf("%s %s returned %s with a JSON body that has no %q list",
				res.HTTPResponse.Request.Method, res.HTTPResponse.Request.URL, res.Status(), "items")
		}
		resp.Diagnostics.AddError(nonJSONSummary, detail)
		return
	}
	out := loadBalancersModel{ClusterID: cfg.ClusterID, LoadBalancers: []loadBalancerModel{}}
	for _, lb := range *wire.Items {
		m := loadBalancerModel{IP: types.StringValue(lb.IP), Purpose: types.StringValue(lb.Purpose),
			ServiceRef: optString(lb.ServiceRef), AllocatedAt: types.StringNull()}
		if lb.AllocatedAt != nil {
			m.AllocatedAt = types.StringValue(lb.AllocatedAt.Format(time.RFC3339))
		}
		out.LoadBalancers = append(out.LoadBalancers, m)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
