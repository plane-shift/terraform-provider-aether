package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/versions"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/wait"
)

// Default operation timeouts, overridable per resource through the `timeouts`
// attribute. Create and Update both wait for a control plane to come (back) up
// to `running`; Delete waits for the cluster to 404.
const (
	clusterCreateTimeout = 30 * time.Minute
	clusterUpdateTimeout = 30 * time.Minute
	clusterDeleteTimeout = 20 * time.Minute
)

var (
	_ resource.Resource                = &clusterResource{}
	_ resource.ResourceWithConfigure   = &clusterResource{}
	_ resource.ResourceWithImportState = &clusterResource{}
	_ resource.ResourceWithModifyPlan  = &clusterResource{}
)

// NewClusterResource returns the aether_cluster resource.
func NewClusterResource() resource.Resource { return &clusterResource{} }

// clusterResource manages one cluster (control plane) of the provider's
// organization. Node pools are separate resources.
type clusterResource struct{ pd *providerData }

// clusterModel is the aether_cluster state; tfsdk tags are the attribute names.
type clusterModel struct {
	ID                        types.String             `tfsdk:"id"`
	Name                      types.String             `tfsdk:"name"`
	Region                    types.String             `tfsdk:"region"`
	K8sVersion                versions.K8sVersionValue `tfsdk:"k8s_version"`
	PublicEndpoint            types.Bool               `tfsdk:"public_endpoint"`
	Plan                      types.String             `tfsdk:"plan"`
	Status                    types.String             `tfsdk:"status"`
	StatusMessage             types.String             `tfsdk:"status_message"`
	Endpoint                  types.String             `tfsdk:"endpoint"`
	ObservedKubernetesVersion types.String             `tfsdk:"observed_kubernetes_version"`
	CapiClusterName           types.String             `tfsdk:"capi_cluster_name"`
	KeycloakRealm             types.String             `tfsdk:"keycloak_realm"`
	HarborProject             types.String             `tfsdk:"harbor_project"`
	CephPool                  types.String             `tfsdk:"ceph_pool"`
	LbSubnet                  types.String             `tfsdk:"lb_subnet"`
	WorkerSubnet              types.String             `tfsdk:"worker_subnet"`
	CreatedAt                 types.String             `tfsdk:"created_at"`
	UpdatedAt                 types.String             `tfsdk:"updated_at"`
	Timeouts                  timeouts.Value           `tfsdk:"timeouts"`
}

// Metadata names the resource aether_cluster.
func (r *clusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

// immutableComputed is a computed attribute the API sets once, at create, and
// never changes: carrying the prior value into the plan is exact, and keeps
// every unrelated update from showing it as "(known after apply)".
func immutableComputed(desc string) schema.StringAttribute {
	return schema.StringAttribute{
		Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
}

// observedComputed is a computed attribute the API legitimately CHANGES over
// the cluster's life (status, endpoint, observed version, updated_at move on
// an upgrade or a rename). It deliberately has NO UseStateForUnknown: pinning
// it to the prior value at plan time promises Terraform the old value, and
// the new one the API returns after apply is then rejected as "Provider
// produced inconsistent result after apply".
func observedComputed(desc string) schema.StringAttribute {
	return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
}

// Schema defines aether_cluster.
func (r *clusterResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed Kubernetes cluster (control plane). Node pools are separate `aether_node_pool` resources. Creation waits until the cluster is `running` (or `degraded`, reported as a warning); a `k8s_version` change performs an in-place upgrade through the upgrade endpoint and must move forward.",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("Cluster UUID."),
			"name": schema.StringAttribute{Required: true,
				MarkdownDescription: "Cluster name. Changing it renames the cluster in place."},
			"region": schema.StringAttribute{Required: true,
				MarkdownDescription: "Region id from `data.aether_regions` (e.g. `eu-fi`). Changing it replaces the cluster.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"k8s_version": schema.StringAttribute{Required: true, CustomType: versions.K8sVersionType{},
				MarkdownDescription: "`MAJOR.MINOR` (resolves to the latest offered patch) or an explicit `MAJOR.MINOR.PATCH` of an offered minor. `1.37` and `1.37.0` are treated as equal. Only forward changes are allowed and go through an upgrade."},
			// Both public_endpoint and plan are IMMUTABLE after create, measured
			// in the vendored spec: UpdateClusterRequest carries public_endpoint
			// "accepted only when it equals the cluster's current value ... any
			// other value is rejected with 422 public_endpoint/immutable", and
			// has NO field for plan at all (Cluster.plan_slug: "Immutable - no
			// endpoint changes it"). So a change to either can only be honoured
			// by replacement; without RequiresReplace the provider plans an
			// in-place update the API will reject.
			//
			// Optional+Computed with a static default has a subtlety:
			// REMOVING public_endpoint from the configuration plans the default
			// (false), not "no change". If state holds true, that is a change,
			// and with RequiresReplace it is a REPLACEMENT - correct per the
			// API, but dropping the attribute is not a no-op, and the
			// description says so.
			"public_endpoint": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
				MarkdownDescription: "Expose the API server on a public IP + DNS name (billed per public IP). Defaults to `false`. Immutable: changing it — including removing it from the configuration while it is `true` — replaces the cluster."},
			// plan is Optional and NOT Computed: what the configuration says is
			// what state holds. Measured in aether-api@8e9045a: an omitted plan
			// is stored NULL and plan_slug is then absent from every response;
			// a set plan is echoed verbatim. An EMPTY string is accepted by the
			// API and also stored as NULL - so `plan = ""` would plan "" and
			// read back null, an inconsistent result after apply. The
			// LengthAtLeast(1) validator refuses it at validation, before any
			// API call; omitting the attribute is how to say "no package".
			"plan": schema.StringAttribute{Optional: true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				MarkdownDescription: "Package slug (`dev`, `starter`, `growth`, `scale`); omit to be billed per resource. Must not be empty. Immutable: changing it replaces the cluster."},
			"status":                      observedComputed("Lifecycle status reported by the API (`provisioning`, `running`, `upgrading`, `degraded`, `failed`, ...)."),
			"status_message":              observedComputed("Human explanation accompanying the status, when the API offers one."),
			"endpoint":                    observedComputed("API server URL, once the cluster is provisioned far enough to report one."),
			"observed_kubernetes_version": observedComputed("Kubernetes version the control plane reports once a rollout settles."),
			"capi_cluster_name":           immutableComputed("Internal Cluster API name of the cluster."),
			"keycloak_realm":              immutableComputed("Identity realm of the cluster."),
			"harbor_project":              immutableComputed("Registry project associated with the cluster."),
			"ceph_pool":                   immutableComputed("Storage pool of the cluster."),
			"lb_subnet":                   immutableComputed("Load-balancer subnet allocated at create time."),
			"worker_subnet":               immutableComputed("Per-cluster worker /24 allocated at create time."),
			"created_at":                  immutableComputed("Creation time (RFC 3339)."),
			"updated_at":                  observedComputed("Last update time (RFC 3339)."),
			"timeouts":                    timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

// Configure receives the provider's client and organization.
func (r *clusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan refuses a backwards k8s_version change before anything is applied.
func (r *clusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy: there is no version to move from or to
	}
	var state, plan clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() || plan.K8sVersion.IsUnknown() || state.K8sVersion.IsNull() {
		return
	}
	// A plan that replaces the cluster creates a NEW one at the configured
	// version, so an older version there is not a downgrade of anything. The
	// framework does not hand the attributes' RequiresReplace verdict to a
	// resource-level ModifyPlan (fwserver passes it an empty path set), so the
	// replacing attributes are compared here directly - an unknown value
	// counts as a change, as it does for RequiresReplace itself.
	if !plan.Region.Equal(state.Region) || !plan.PublicEndpoint.Equal(state.PublicEndpoint) || !plan.Plan.Equal(state.Plan) {
		return
	}
	from, to := state.K8sVersion.ValueString(), plan.K8sVersion.ValueString()
	if versions.SemanticallyEqual(to, from) {
		return
	}
	fwd, err := versions.IsForward(from, to)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("k8s_version"), "Invalid k8s_version", err.Error())
		return
	}
	if !fwd {
		resp.Diagnostics.AddAttributeError(path.Root("k8s_version"), "Kubernetes downgrade is not supported",
			fmt.Sprintf("k8s_version would move backwards from %s to %s. Aether only upgrades clusters; recreate the cluster to run an older version.", from, to))
	}
}

// Create creates the cluster and waits for it to reach running (or degraded).
func (r *clusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Read the timeout BEFORE the API call: carrying on past an error here
	// would create a cluster and then wait on it with a zero timeout.
	createTimeout, d := plan.Timeouts.Create(ctx, clusterCreateTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := client.CreateClusterJSONRequestBody{
		Name: plan.Name.ValueString(), Region: plan.Region.ValueString(), K8sVersion: plan.K8sVersion.ValueString(),
	}
	if !plan.PublicEndpoint.IsNull() && !plan.PublicEndpoint.IsUnknown() {
		v := plan.PublicEndpoint.ValueBool()
		body.PublicEndpoint = &v
	}
	if !plan.Plan.IsNull() && !plan.Plan.IsUnknown() {
		v := plan.Plan.ValueString()
		body.Plan = &v
	}
	res, err := r.pd.Client.CreateClusterWithResponse(ctx, r.pd.orgID, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create cluster", err.Error())
		return
	}
	// createCluster documents **202 only** ("Cluster accepted for
	// provisioning", api/openapi.yaml), and CreateClusterResponse carries a
	// single typed success field, JSON202 - there is no JSON200 or JSON201 to
	// fall back on. Accepting 200/201 here would take a body the parser never
	// populated.
	if res.StatusCode() != http.StatusAccepted {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	// Use the TYPED body and guard it. An id must never come from an
	// unchecked decode: a 202 whose body is not the expected JSON leaves a
	// ZERO UUID, and persisting it records
	// 00000000-0000-0000-0000-000000000000 while the cluster that was really
	// created is orphaned, unreferenced by any state and un-importable without
	// hunting for it in the portal. Failing here instead leaves no state entry
	// at all, which is the recoverable outcome.
	created := res.JSON202
	if created == nil {
		resp.Diagnostics.AddError("Aether API accepted the cluster but returned no usable body",
			fmt.Sprintf("POST %s returned %s with content type %q, not JSON. The cluster may exist; check the portal before retrying, since a retry creates a second one.",
				res.HTTPResponse.Request.URL, res.Status(), res.HTTPResponse.Header.Get("Content-Type")))
		return
	}
	plan.ID = types.StringValue(created.Id.String()) // UUID -> canonical text for state
	// Persist the id immediately, with the create response flattened in, so a
	// wait failure leaves an importable (tainted) resource in state rather than
	// one whose computed attributes are still unknown.
	r.flatten(created, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	final, err := r.waitForCluster(ctx, created.Id, createTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not become running", err.Error())
		return
	}
	r.flatten(final.Cluster, &plan)
	if final.Warned {
		resp.Diagnostics.AddWarning("Cluster is degraded", final.Message)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the cluster; a 404 removes it from state so the next plan
// re-creates it.
func (r *clusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid cluster id in state", err.Error())
		return
	}
	res, err := r.pd.Client.GetClusterWithResponse(ctx, r.pd.orgID, clusterID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read cluster", err.Error())
		return
	}
	if res.StatusCode() == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}
	if res.StatusCode() != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	if res.JSON200 == nil {
		resp.Diagnostics.AddError("Aether API answered 200 without a JSON body",
			fmt.Sprintf("GET %s returned %s with content type %q.",
				res.HTTPResponse.Request.URL, res.Status(), res.HTTPResponse.Header.Get("Content-Type")))
		return
	}
	r.flatten(res.JSON200, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update renames through PUT and upgrades through the upgrade endpoint, then
// waits for the cluster to return to running (or degraded).
func (r *clusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	updateTimeout, d := plan.Timeouts.Update(ctx, clusterUpdateTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid cluster id in state", err.Error())
		return
	}

	// 1. mutable scalar fields via PUT.
	//
	// `name` is the ONLY field Update sends. public_endpoint and plan are
	// immutable (see the schema) and carry RequiresReplace, so a change to
	// either never reaches Update at all - Terraform destroys and recreates
	// instead. Sending public_endpoint here would be either a no-op (the API
	// accepts it only at its current value) or a guaranteed 422
	// public_endpoint/immutable; omitting it is the only shape that is right
	// in both cases. k8s_version has its own endpoint, below.
	if !plan.Name.Equal(state.Name) {
		name := plan.Name.ValueString()
		res, err := r.pd.Client.UpdateClusterWithResponse(ctx, r.pd.orgID, clusterID, client.UpdateClusterJSONRequestBody{Name: &name})
		if err != nil {
			resp.Diagnostics.AddError("Cannot update cluster", err.Error())
			return
		}
		if res.StatusCode() != http.StatusOK {
			apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
			return
		}
	}
	// 2. k8s_version via the upgrade endpoint (ModifyPlan already refused
	// backwards moves). The response body is not used - the wait below reads
	// the cluster fresh - so any 2xx is success.
	if !versions.SemanticallyEqual(plan.K8sVersion.ValueString(), state.K8sVersion.ValueString()) {
		res, err := r.pd.Client.UpgradeClusterWithResponse(ctx, r.pd.orgID, clusterID,
			client.UpgradeClusterJSONRequestBody{TargetVersion: plan.K8sVersion.ValueString()})
		if err != nil {
			resp.Diagnostics.AddError("Cannot upgrade cluster", err.Error())
			return
		}
		if code := res.StatusCode(); code < 200 || code >= 300 {
			apierr.ToDiagnostics(apierr.Parse(code, res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Root("k8s_version"))
			return
		}
	}
	final, err := r.waitForCluster(ctx, clusterID, updateTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not return to running", err.Error())
		return
	}
	plan.ID = state.ID
	r.flatten(final.Cluster, &plan)
	if final.Warned {
		resp.Diagnostics.AddWarning("Cluster is degraded", final.Message)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete deletes the cluster and waits until the API answers 404 for it.
func (r *clusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteTimeout, d := state.Timeouts.Delete(ctx, clusterDeleteTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid cluster id in state", err.Error())
		return
	}
	res, err := r.pd.Client.DeleteClusterWithResponse(ctx, r.pd.orgID, clusterID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete cluster", err.Error())
		return
	}
	// 200 and 202 are both documented; 404 means it is already gone, which is
	// what Delete wants - the wait below confirms it on its first probe.
	if code := res.StatusCode(); code != http.StatusNotFound && (code < 200 || code >= 300) {
		apierr.ToDiagnostics(apierr.Parse(code, res.HTTPResponse.Header, res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	err = wait.ForGone(ctx, func(ctx context.Context) (bool, error) {
		g, err := r.pd.Client.GetClusterWithResponse(ctx, r.pd.orgID, clusterID)
		if err != nil {
			return false, err
		}
		// Only 404 means gone and only a 2xx means still present. Folding
		// everything else into "still present" turns a 403 (token rotated
		// mid-delete) or a 500 into a silent wait to the full delete timeout,
		// and the diagnostic then blames the timeout instead of naming the
		// error that was in hand on the first probe.
		switch code := g.StatusCode(); {
		case code == http.StatusNotFound:
			return true, nil
		case code >= 200 && code < 300:
			return false, nil
		default:
			return false, apierr.Parse(code, g.HTTPResponse.Header, g.Body)
		}
	}, wait.Options{Timeout: deleteTimeout})
	if err != nil {
		resp.Diagnostics.AddError("Cluster deletion did not complete", err.Error())
	}
}

// ImportState imports a cluster by its UUID; Read fills in the rest.
func (r *clusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// clusterWait is the outcome of waitForCluster: the last cluster read, and
// whether it ended in a warning status (with the API's message).
type clusterWait struct {
	Cluster *client.Cluster
	Message string
	Warned  bool
}

// waitForCluster polls the cluster until running (success), degraded
// (terminal, with a warning) or failed (an error carrying status_message).
func (r *clusterResource) waitForCluster(ctx context.Context, id client.ClusterID, timeout time.Duration) (clusterWait, error) {
	var last *client.Cluster
	res, err := wait.ForStatus(ctx, func(ctx context.Context) (string, string, error) {
		g, err := r.pd.Client.GetClusterWithResponse(ctx, r.pd.orgID, id)
		if err != nil {
			return "", "", err
		}
		if g.StatusCode() != http.StatusOK {
			return "", "", apierr.Parse(g.StatusCode(), g.HTTPResponse.Header, g.Body)
		}
		if g.JSON200 == nil {
			// A 200 that is not JSON is not a status reading. Returning it as
			// an empty status would be treated as "not terminal yet" and poll
			// until the timeout.
			return "", "", fmt.Errorf("GET %s returned %s with content type %q, not JSON",
				g.HTTPResponse.Request.URL, g.Status(), g.HTTPResponse.Header.Get("Content-Type"))
		}
		last = g.JSON200
		return string(last.Status), deref(last.StatusMessage), nil
	}, wait.Options{Timeout: timeout, Success: []string{"running"}, Warning: []string{"degraded"}, Failed: []string{"failed"}})
	if err != nil {
		return clusterWait{}, err
	}
	return clusterWait{Cluster: last, Message: res.Message, Warned: res.Warned}, nil
}

// flatten copies an API cluster into the model. Timeouts is left untouched:
// it is configuration, not something the API reports.
func (r *clusterResource) flatten(c *client.Cluster, m *clusterModel) {
	m.ID = types.StringValue(c.Id.String())
	m.Name = types.StringValue(c.Name)
	m.Region = types.StringValue(c.Region)
	// Keep the configured spelling when it is semantically equal to what the
	// API holds ("1.37" configured, "1.37.4" resolved), otherwise take the
	// API's value so real drift shows.
	if m.K8sVersion.IsNull() || m.K8sVersion.IsUnknown() || !versions.SemanticallyEqual(m.K8sVersion.ValueString(), c.K8sVersion) {
		m.K8sVersion = versions.NewK8sVersionValue(c.K8sVersion)
	}
	// Cluster.public_endpoint is *bool and NOT in the schema's `required`
	// list (measured in api/openapi.yaml), so it can legitimately be absent.
	// Absent maps to FALSE rather than null, on purpose: the attribute is
	// Optional+Computed with Default false, so a null here would contradict a
	// planned false and produce "Provider produced inconsistent result after
	// apply". The API's own reading agrees - a cluster with no public endpoint
	// reads false (the gap it calls I-14), not a third state. Do NOT use
	// types.BoolPointerValue here, for that reason.
	m.PublicEndpoint = types.BoolValue(c.PublicEndpoint != nil && *c.PublicEndpoint)
	m.Plan = optString(c.PlanSlug)
	m.Status = types.StringValue(string(c.Status))
	m.StatusMessage = optString(c.StatusMessage)
	m.Endpoint = optString(c.Endpoint)
	m.ObservedKubernetesVersion = optString(c.ObservedKubernetesVersion)
	m.CapiClusterName = optString(c.CapiClusterName)
	m.KeycloakRealm = optString(c.KeycloakRealm)
	m.HarborProject = optString(c.HarborProject)
	m.CephPool = optString(c.CephPool)
	m.LbSubnet = optString(c.LbSubnet)
	m.WorkerSubnet = optString(c.WorkerSubnet)
	m.CreatedAt = types.StringValue(c.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(c.UpdatedAt.Format(time.RFC3339))
}
