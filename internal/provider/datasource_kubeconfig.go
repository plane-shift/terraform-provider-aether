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
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
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
			"cluster_id": schema.StringAttribute{Required: true, Validators: []validator.String{canonicalIDValidator{}}, MarkdownDescription: "UUID of the cluster."},
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
		resp.Diagnostics.AddError("Kubeconfig not available", kubeconfigConflictDetail(e))
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

// kubeconfigConflictDetail renders the API's 409, which has two causes:
// the cluster is not `running` ("Cluster is not yet ready. Current status:
// upgrading") - named with the current status - or the cluster IS running but
// its control plane has not published a kubeconfig yet, which is reported in
// the API's own words (so it never claims the cluster is not running). The
// request id is appended exactly as apierr.ToDiagnostics does.
func kubeconfigConflictDetail(e *apierr.Error) string {
	reason := " (the API said: " + apierr.Truncate(e.Message, 200) + ")"
	const marker = "Current status:"
	if i := strings.Index(e.Message, marker); i >= 0 {
		if st := strings.TrimSpace(e.Message[i+len(marker):]); st != "" {
			reason = ": it is available only while it is `running` (current status: " + apierr.Truncate(st, 64) + ")"
		}
	}
	rid := ""
	if e.RequestID != "" {
		rid = " (request id " + e.RequestID + ")"
	}
	return "The cluster's kubeconfig is not available yet" + reason + rid + "."
}
