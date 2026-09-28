package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &kubeconfigDataSource{}

// NewKubeconfigDataSource returns data.aether_kubeconfig.
func NewKubeconfigDataSource() datasource.DataSource { return &kubeconfigDataSource{} }

type kubeconfigDataSource struct{ pd *providerData }

type kubeconfigModel struct {
	ClusterID types.String `tfsdk:"cluster_id"`
	Content   types.String `tfsdk:"content"`
}

// maxKubeconfigBytes bounds the body read: an admin kubeconfig is a few KB,
// and a body past this is not one.
const maxKubeconfigBytes = 1 << 20

func (d *kubeconfigDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kubeconfig"
}

func (d *kubeconfigDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The cluster's administrator kubeconfig, the same file the portal downloads. It grants cluster-admin: treat the Terraform state as a secret. It is available only while the cluster is `running`; a plan that reads it fails while the cluster is upgrading or degraded.",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{Required: true, MarkdownDescription: "UUID of the cluster."},
			"content": schema.StringAttribute{Computed: true, Sensitive: true,
				MarkdownDescription: "The kubeconfig YAML, byte for byte as the API serves it."},
		},
	}
}

func (d *kubeconfigDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *kubeconfigDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg kubeconfigModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, ok := parseInputID(path.Root("cluster_id"), cfg.ClusterID.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	// The raw call, NOT GetKubeconfigWithResponse: the API serves the file as
	// application/yaml and the generated parser YAML-decodes that mapping
	// into a Go string, which fails on every 200 (aether-api issue #316).
	hr, err := d.pd.Client.GetKubeconfig(ctx, d.pd.orgID, clusterID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the kubeconfig", err.Error())
		return
	}
	defer hr.Body.Close()
	body, err := io.ReadAll(io.LimitReader(hr.Body, maxKubeconfigBytes+1))
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the kubeconfig", fmt.Sprintf("Reading the response body of GET %s: %s", hr.Request.URL, err))
		return
	}
	switch {
	case hr.StatusCode == http.StatusConflict:
		e := apierr.Parse(hr.StatusCode, hr.Header, body)
		resp.Diagnostics.AddError("Kubeconfig not available",
			fmt.Sprintf("The cluster's kubeconfig is available only while it is `running` (%s).", notReadyDetail(e.Message)))
		return
	case hr.StatusCode != http.StatusOK:
		apierr.ToDiagnostics(apierr.Parse(hr.StatusCode, hr.Header, body), &resp.Diagnostics, path.Empty())
		return
	}
	if len(body) > maxKubeconfigBytes {
		resp.Diagnostics.AddError("Aether API answered 200 without a kubeconfig",
			fmt.Sprintf("GET %s returned a body larger than %d bytes, which is not a kubeconfig.", hr.Request.URL, maxKubeconfigBytes))
		return
	}
	if len(bytes.TrimSpace(body)) == 0 || !bytes.Contains(body, []byte("apiVersion")) {
		resp.Diagnostics.AddError("Aether API answered 200 without a kubeconfig",
			fmt.Sprintf("GET %s returned %s with content type %q and a body that is not a kubeconfig (%d bytes, no `apiVersion`).",
				hr.Request.URL, hr.Status, hr.Header.Get("Content-Type"), len(body)))
		return
	}
	out := kubeconfigModel{ClusterID: cfg.ClusterID, Content: types.StringValue(string(body))}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

// notReadyDetail turns the API's 409 message into the parenthetical of the
// "available only while running" error: the current status when the API
// names it ("Cluster is not yet ready. Current status: upgrading"), the API's
// own words otherwise (a running cluster whose control plane has not
// published a kubeconfig yet).
func notReadyDetail(msg string) string {
	const marker = "Current status:"
	if i := strings.Index(msg, marker); i >= 0 {
		if s := strings.TrimSpace(msg[i+len(marker):]); s != "" {
			return "current status: " + apierr.Truncate(s, 64)
		}
	}
	return "the API said: " + apierr.Truncate(msg, 200)
}
