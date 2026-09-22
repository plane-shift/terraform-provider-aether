package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/wait"
)

// Default operation timeouts, overridable through the `timeouts` attribute.
// Create waits for the pool's nodes to come up; Update waits only for a
// fixed pool's node-count change to converge; Delete waits for the pool to 404.
const (
	nodePoolCreateTimeout = 30 * time.Minute
	nodePoolUpdateTimeout = 30 * time.Minute
	nodePoolDeleteTimeout = 20 * time.Minute
)

// Node-pool statuses the API really emits (aether-api@8e9045a reconciler;
// the OpenAPI enum's `ready` and `failed` never occur, so the generated
// client.NodePoolStatus constants are not used).
const (
	poolStatusRunning  = "running"
	poolStatusDegraded = "degraded"
	poolStatusDeleting = "deleting"
)

// poolNameRE mirrors the API's name validation (contract §3,
// internal/model/name.go:97-122 in aether-api@8e9045a): starts and ends
// with a lowercase letter or digit, `-` allowed only in between. Plan-time
// only - the length check stays a separate validator, and the API's
// composed-name 51-character check has no plan-time equivalent.
var poolNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

var (
	_ resource.Resource                = &nodePoolResource{}
	_ resource.ResourceWithConfigure   = &nodePoolResource{}
	_ resource.ResourceWithImportState = &nodePoolResource{}
	_ resource.ResourceWithModifyPlan  = &nodePoolResource{}
)

// NewNodePoolResource returns the aether_node_pool resource.
func NewNodePoolResource() resource.Resource { return &nodePoolResource{} }

// nodePoolResource manages one node pool of a cluster in the provider's
// organization.
type nodePoolResource struct{ pd *providerData }

// nodePoolModel is the aether_node_pool state; tfsdk tags are the attribute names.
type nodePoolModel struct {
	ID                          types.String   `tfsdk:"id"`
	ClusterID                   types.String   `tfsdk:"cluster_id"`
	Name                        types.String   `tfsdk:"name"`
	Vcpus                       types.Int64    `tfsdk:"vcpus"`
	MemoryGb                    types.Int64    `tfsdk:"memory_gb"`
	DiskGb                      types.Int64    `tfsdk:"disk_gb"`
	DesiredNodes                types.Int64    `tfsdk:"desired_nodes"`
	MinNodes                    types.Int64    `tfsdk:"min_nodes"`
	MaxNodes                    types.Int64    `tfsdk:"max_nodes"`
	MinVcpus                    types.Int64    `tfsdk:"min_vcpus"`
	MaxVcpus                    types.Int64    `tfsdk:"max_vcpus"`
	MinMemoryGb                 types.Int64    `tfsdk:"min_memory_gb"`
	MaxMemoryGb                 types.Int64    `tfsdk:"max_memory_gb"`
	NodeLabels                  types.Map      `tfsdk:"node_labels"`
	NodeTaints                  types.List     `tfsdk:"node_taints"`
	DrainTimeoutSeconds         types.Int64    `tfsdk:"drain_timeout_seconds"`
	ScaleDownDelaySeconds       types.Int64    `tfsdk:"scale_down_delay_seconds"`
	ScaleDownUtilizationPercent types.Int64    `tfsdk:"scale_down_utilization_percent"`
	CurrentNodes                types.Int64    `tfsdk:"current_nodes"`
	Status                      types.String   `tfsdk:"status"`
	StatusMessage               types.String   `tfsdk:"status_message"`
	FirstReadyAt                types.String   `tfsdk:"first_ready_at"`
	CreatedAt                   types.String   `tfsdk:"created_at"`
	UpdatedAt                   types.String   `tfsdk:"updated_at"`
	Timeouts                    timeouts.Value `tfsdk:"timeouts"`
}

// nodeTaintModel is one element of node_taints.
type nodeTaintModel struct {
	Key    types.String `tfsdk:"key"`
	Value  types.String `tfsdk:"value"`
	Effect types.String `tfsdk:"effect"`
}

// nodeTaintType is the object type of a node_taints element; it must match
// the nested attributes declared in Schema.
var nodeTaintType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"key": types.StringType, "value": types.StringType, "effect": types.StringType,
}}

// Metadata names the resource aether_node_pool.
func (r *nodePoolResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_node_pool"
}

// replaceInt64 is a Required node-size attribute that forces replacement.
func replaceInt64(desc string, v validator.Int64) schema.Int64Attribute {
	return schema.Int64Attribute{Required: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
		Validators:    []validator.Int64{v}}
}

// budgetInt64 is one of the four autoscaling budget attributes. Optional +
// Computed with Default 0: the API always echoes 0 for "no budget", and
// removing the attribute from the configuration plans 0, which Update sends
// explicitly - an omitted field keeps the stored value on PUT.
func budgetInt64(desc string) schema.Int64Attribute {
	return schema.Int64Attribute{Optional: true, Computed: true, Default: int64default.StaticInt64(0),
		MarkdownDescription: desc, Validators: []validator.Int64{int64validator.AtLeast(0)}}
}

// keptKnobInt64 is a scale-down knob. The API cannot clear it once set
// (omitted and null both keep the stored value), so it is Optional+Computed
// with UseStateForUnknown: removing it from the configuration plans the stored
// value - the truth - instead of a null the API will never report.
func keptKnobInt64(desc string, v validator.Int64) schema.Int64Attribute {
	return schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: desc,
		PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
		Validators:    []validator.Int64{v}}
}

// Schema defines aether_node_pool.
func (r *nodePoolResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A pool of identically sized worker nodes in an `aether_cluster`. " +
			"A pool is FIXED (billed as a flat monthly fee, `desired_nodes` nodes) unless its budget allows growth (a `max_vcpus` above `min_vcpus` or a `max_memory_gb` above `min_memory_gb`), which makes it AUTOSCALING (billed hourly per node, node count chosen by the autoscaler inside the budget). " +
			"Creation waits until the pool is `running`. On update, only a node-count change on a fixed pool is waited for (until every desired node is ready); every other change returns once the API has accepted it, because the platform exposes no signal that it has been applied.",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("Node pool UUID."),
			"cluster_id": schema.StringAttribute{Required: true,
				MarkdownDescription: "UUID of the `aether_cluster` the pool belongs to. Changing it replaces the pool.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			// The API renames in place, but the platform keys everything below
			// the API on the name: a rename creates a new machine set and deletes
			// the old one in the same pass with no capacity overlap (measured,
			// aether-controllers@f2f515d). Replacement is the honest plan.
			"name": schema.StringAttribute{Required: true,
				MarkdownDescription: "Pool name (lowercase letters, digits and `-`, at most 40 characters). Changing it REPLACES the pool: the platform would destroy and recreate every node on a rename anyway, so the plan says so. The old pool is deleted before the new one is created, because a pool keeps its name until its deletion completes. Under `lifecycle { create_before_destroy = true }` a same-name replacement (e.g. a `vcpus`/`memory_gb`/`disk_gb` change) fails with the API's 409 \"Node pool already exists\", because the old pool is still LIVE (not yet deleted) and still holds its name when the new one's create runs.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators: []validator.String{stringvalidator.LengthBetween(1, 40),
					stringvalidator.RegexMatches(poolNameRE, "must start and end with a lowercase letter or digit, and contain only lowercase letters, digits and `-` in between")}},
			// The API accepts a size change, but it never reaches a machine: the
			// machine template is written once at pool creation and never
			// updated, so existing AND future nodes keep the old size while the
			// API reports the new one (aether-controllers@f2f515d). Only a new
			// pool gets the new size.
			"vcpus": replaceInt64("vCPUs per node (1-128). Changing it REPLACES the pool: the platform never applies a size change to any existing or future node of a pool, so only a new pool gets the new size.",
				int64validator.Between(1, 128)),
			"memory_gb": replaceInt64("Memory per node in GB (1-512). Changing it REPLACES the pool, for the same reason as `vcpus`.",
				int64validator.Between(1, 512)),
			"disk_gb": replaceInt64("Disk per node in GB (1-2048). Changing it REPLACES the pool, for the same reason as `vcpus`.",
				int64validator.Between(1, 2048)),
			"desired_nodes": schema.Int64Attribute{Optional: true, Computed: true,
				MarkdownDescription: "Number of nodes. Defaults to 1 at create. On a fixed pool a change scales the pool, and apply waits until that many nodes are ready. On an AUTOSCALING pool it only seeds the initial size: after create the platform ignores it (the autoscaler owns the node count), so changing it is stored but changes nothing.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
				Validators:          []validator.Int64{int64validator.AtLeast(0)}},
			// min_nodes/max_nodes are planned from desired_nodes in ModifyPlan
			// when the configuration leaves them null, NOT UseStateForUnknown:
			// scaling 1 -> 3 would then plan {min 1, desired 3, max 1}, which the
			// API refuses with 422.
			"min_nodes": schema.Int64Attribute{Optional: true, Computed: true,
				MarkdownDescription: "Lower node bound, `min_nodes <= desired_nodes <= max_nodes`. When omitted it follows `desired_nodes`. The platform derives an autoscaling pool's real bounds from its budget, not from this value.",
				Validators:          []validator.Int64{int64validator.AtLeast(0)}},
			"max_nodes": schema.Int64Attribute{Optional: true, Computed: true,
				MarkdownDescription: "Upper node bound. When omitted it follows `desired_nodes`. The platform derives an autoscaling pool's real bounds from its budget, not from this value.",
				Validators:          []validator.Int64{int64validator.AtLeast(0)}},
			"min_vcpus":     budgetInt64("Autoscaling budget: minimum total vCPUs of the pool. `0` (the default) means no budget. All four budget attributes at `0` make a FIXED pool, billed as a flat monthly fee; a `max_vcpus` above `min_vcpus` (or `max_memory_gb` above `min_memory_gb`) makes the pool AUTOSCALING, billed hourly per node. A non-zero minimum must cover at least one node. Removing the attribute sets it back to `0`."),
			"max_vcpus":     budgetInt64("Autoscaling budget: maximum total vCPUs of the pool (at least `vcpus` when set). `0` (the default) means no budget; see `min_vcpus`. Removing the attribute sets it back to `0`."),
			"min_memory_gb": budgetInt64("Autoscaling budget: minimum total memory of the pool in GB. `0` (the default) means no budget; see `min_vcpus`. Removing the attribute sets it back to `0`."),
			"max_memory_gb": budgetInt64("Autoscaling budget: maximum total memory of the pool in GB (at least `memory_gb` when set). `0` (the default) means no budget; see `min_vcpus`. Removing the attribute sets it back to `0`."),
			// Labels and taints are not in the API's update request at all
			// (sending either is a 400), so they can only change by replacement.
			// The API echoes {} / [] when none were set; replacing a pool over
			// "null became empty" (or back) would destroy nodes for nothing, so
			// the replace fires only when the contents really differ.
			"node_labels": schema.MapAttribute{Optional: true, ElementType: types.StringType,
				MarkdownDescription: "Kubernetes labels applied to every node. Keys under `aether.cloud/`, `kubernetes.io/`, `k8s.io/`, `node-role.kubernetes.io/` and `node.kubernetes.io/` are reserved. Immutable: changing them replaces the pool.",
				PlanModifiers: []planmodifier.Map{mapplanmodifier.RequiresReplaceIf(mapContentChanged,
					"Node labels cannot be changed on an existing pool.", "Node labels cannot be changed on an existing pool.")}},
			"node_taints": schema.ListNestedAttribute{Optional: true,
				MarkdownDescription: "Kubernetes taints applied to every node. A `NoSchedule` or `NoExecute` taint needs another untainted pool in the cluster. Immutable: changing them replaces the pool.",
				PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplaceIf(listContentChanged,
					"Node taints cannot be changed on an existing pool.", "Node taints cannot be changed on an existing pool.")},
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"key": schema.StringAttribute{Required: true, MarkdownDescription: "Taint key.",
						Validators: []validator.String{stringvalidator.LengthAtLeast(1)}},
					// The API echoes "value":"" for a taint configured without
					// one, so the default is "" rather than null.
					"value": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""),
						MarkdownDescription: "Taint value; defaults to empty."},
					"effect": schema.StringAttribute{Required: true,
						MarkdownDescription: "`NoSchedule`, `PreferNoSchedule` or `NoExecute`.",
						Validators:          []validator.String{stringvalidator.OneOf("NoSchedule", "PreferNoSchedule", "NoExecute")}},
				}}},
			// Optional and NOT Computed: null means "track the platform default"
			// and is never sent. Once set, the API cannot clear it (omitted and
			// null both keep the stored value), so ModifyPlan refuses a removal.
			"drain_timeout_seconds": schema.Int64Attribute{Optional: true,
				MarkdownDescription: "How long a node being removed may take to drain, 30-3600 seconds. Omit it to track the platform default (currently 600). **Once set it cannot be removed**: the API has no way to clear it, so removing it from the configuration is refused at plan time. Set an explicit value instead (e.g. `600`), or recreate the pool: taint it (`terraform taint`/`tofu taint`) then apply with the attribute removed - `-replace` and `replace_triggered_by` hit the same refusal and do not work here.",
				Validators:          []validator.Int64{int64validator.Between(30, 3600)}},
			"scale_down_delay_seconds": keptKnobInt64("Autoscaling: how long a node must be unneeded before it is removed, 60-3600 seconds. Omit it for the platform default (currently 600). Once set, the API cannot clear it: removing it from the configuration keeps the stored value.",
				int64validator.Between(60, 3600)),
			"scale_down_utilization_percent": keptKnobInt64("Autoscaling: utilization below which a node counts as unneeded, 10-90 percent. Omit it for the platform default (currently 50). Once set, the API cannot clear it: removing it from the configuration keeps the stored value.",
				int64validator.Between(10, 90)),
			"current_nodes": schema.Int64Attribute{Computed: true,
				MarkdownDescription: "Number of ready nodes the platform reports. On an autoscaling pool it moves with the autoscaler, which is not drift."},
			"status":         observedComputed("Lifecycle status reported by the API: `provisioning`, `running`, `scaling`, `scaling-down`, `degraded` (with `status_message`; clears by itself once the nodes recover) or `deleting`."),
			"status_message": observedComputed("Explanation accompanying a `degraded` status."),
			"first_ready_at": observedComputed("Time the pool first had a ready node (RFC 3339); null until then."),
			"created_at":     immutableComputed("Creation time (RFC 3339)."),
			"updated_at":     observedComputed("Last update time (RFC 3339). The platform moves it without any API call."),
			"timeouts":       timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

// mapContentChanged is the RequiresReplaceIf test for node_labels: replace
// only when the labels really differ. Null and an empty map are the same
// labels - the API echoes {} for "none" - so a move between them (an imported
// pool, or a configuration that starts spelling `{}`) is not a reason to
// destroy nodes; Update handles it without an API call.
func mapContentChanged(_ context.Context, req planmodifier.MapRequest, resp *mapplanmodifier.RequiresReplaceIfFuncResponse) {
	if len(req.StateValue.Elements()) == 0 && len(req.PlanValue.Elements()) == 0 && !req.PlanValue.IsUnknown() {
		return
	}
	resp.RequiresReplace = !req.StateValue.Equal(req.PlanValue)
}

// listContentChanged is mapContentChanged for node_taints.
func listContentChanged(_ context.Context, req planmodifier.ListRequest, resp *listplanmodifier.RequiresReplaceIfFuncResponse) {
	if len(req.StateValue.Elements()) == 0 && len(req.PlanValue.Elements()) == 0 && !req.PlanValue.IsUnknown() {
		return
	}
	resp.RequiresReplace = !req.StateValue.Equal(req.PlanValue)
}

// Configure receives the provider's client and organization.
func (r *nodePoolResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan does three things the attribute plan modifiers cannot:
//
//  1. min_nodes / max_nodes left null in the configuration are planned EQUAL
//     to the planned desired_nodes, mirroring the API's create default. They
//     are Computed, so setting the planned value is legal. It has to happen
//     here, after the attribute modifiers, because it reads desired_nodes'
//     FINAL planned value (UseStateForUnknown has already run on it).
//  2. When that rewrite (or an import's stored bounds) leaves min_nodes or
//     max_nodes different from state, the four attributes an Update's
//     PUT+re-read can move (updated_at, status, status_message,
//     current_nodes) are marked unknown in the plan - see the comment below.
//  3. Removing drain_timeout_seconds after it was set is refused: the API
//     cannot clear it, so the plan would promise a null the API never
//     reports. A replacement is never chosen silently instead.
func (r *nodePoolResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan, config nodePoolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if config.MinNodes.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("min_nodes"), plan.DesiredNodes)...)
	}
	if config.MaxNodes.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("max_nodes"), plan.DesiredNodes)...)
	}
	if req.State.Raw.IsNull() {
		return // create: nothing was set before
	}
	var state nodePoolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// I-2 (review round 2, blocking): the framework only marks null-in-config
	// Computed attributes unknown when Terraform core's proposed plan (built
	// BEFORE this method runs) already differs from the prior state
	// SOMEWHERE in the object (fwserver/server_planresourcechange.go
	// ~205-252, v1.19.0). Removing min_nodes/max_nodes from config, or
	// importing a pool whose stored bounds differ from desired_nodes, gives a
	// proposed plan IDENTICAL to the prior state at that point - Optional+
	// Computed with null config keeps the prior value - so updated_at,
	// status, status_message and current_nodes all stay planned at their
	// prior KNOWN values. The rewrite above then moves min_nodes/max_nodes,
	// Update sends a PUT, and the real re-read moves those four - "provider
	// produced inconsistent result after apply" (measured). Whenever the
	// FINAL planned min/max (after the rewrite) differs from state, defer
	// those four to the real post-apply read, exactly as a create would.
	// first_ready_at, id and created_at are excluded: nothing a node-count
	// PUT does can move them.
	finalMin, finalMax := plan.MinNodes, plan.MaxNodes
	if config.MinNodes.IsNull() {
		finalMin = plan.DesiredNodes
	}
	if config.MaxNodes.IsNull() {
		finalMax = plan.DesiredNodes
	}
	if !finalMin.Equal(state.MinNodes) || !finalMax.Equal(state.MaxNodes) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("updated_at"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status_message"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("current_nodes"), types.Int64Unknown())...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	if state.DrainTimeoutSeconds.IsNull() || !plan.DrainTimeoutSeconds.IsNull() {
		return
	}
	// A plan that replaces the pool creates a NEW one without the setting,
	// which is fine. The framework does not hand the attributes'
	// RequiresReplace verdict to a resource-level ModifyPlan, so the replacing
	// attributes are compared here directly (labels/taints by content, as
	// their RequiresReplaceIf does); an unknown value counts as a change.
	if !plan.ClusterID.Equal(state.ClusterID) || !plan.Name.Equal(state.Name) || !plan.Vcpus.Equal(state.Vcpus) ||
		!plan.MemoryGb.Equal(state.MemoryGb) || !plan.DiskGb.Equal(state.DiskGb) ||
		contentDiffers(len(plan.NodeLabels.Elements()), len(state.NodeLabels.Elements()), plan.NodeLabels.IsUnknown(), plan.NodeLabels.Equal(state.NodeLabels)) ||
		contentDiffers(len(plan.NodeTaints.Elements()), len(state.NodeTaints.Elements()), plan.NodeTaints.IsUnknown(), plan.NodeTaints.Equal(state.NodeTaints)) {
		return
	}
	resp.Diagnostics.AddAttributeError(path.Root("drain_timeout_seconds"), "drain_timeout_seconds cannot be removed",
		fmt.Sprintf("drain_timeout_seconds is set to %d on this pool, and the Aether API has no way to clear it back to the platform default (currently 600 seconds). "+
			"Keep an explicit value instead, or recreate the pool: taint it first - `terraform taint <this resource address>` (or `tofu taint <this resource address>`) - "+
			"then apply with the attribute removed; a tainted object is planned with no prior state, so this refusal does not fire on that apply. "+
			"`terraform apply -replace=<address>` and `lifecycle { replace_triggered_by = [...] }` do NOT work here: Terraform plans the forced replacement only after a first "+
			"plan against the real prior state, which hits this same refusal and returns before the replace decision is made (read from Terraform core's source; not exercised by a test here).",
			state.DrainTimeoutSeconds.ValueInt64()))
}

// contentDiffers mirrors mapContentChanged/listContentChanged for
// ModifyPlan: both sides empty (null or zero elements) is no change, and an
// unknown planned value counts as one.
func contentDiffers(planLen, stateLen int, planUnknown, equal bool) bool {
	if planUnknown {
		return true
	}
	if planLen == 0 && stateLen == 0 {
		return false
	}
	return !equal
}

// Create creates the pool and waits for it to reach running.
func (r *nodePoolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan nodePoolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Read the timeout BEFORE the API call: carrying on past an error here
	// would create a pool and then wait on it with a zero timeout.
	createTimeout, d := plan.Timeouts.Create(ctx, nodePoolCreateTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, err := parseID(plan.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("cluster_id"), "Invalid cluster_id", err.Error())
		return
	}

	body := client.CreateNodePoolJSONRequestBody{
		Name: plan.Name.ValueString(), Vcpus: int(plan.Vcpus.ValueInt64()),
		MemoryGb: int(plan.MemoryGb.ValueInt64()), DiskGb: int(plan.DiskGb.ValueInt64()),
		DesiredNodes: knownInt(plan.DesiredNodes), MinNodes: knownInt(plan.MinNodes), MaxNodes: knownInt(plan.MaxNodes),
		// Omitted, null and 0 are the same budget to the API (a plain int with
		// omitempty on its side). A 0 is left out rather than sent because the
		// vendored spec still declares the max fields `minimum: 1`.
		MinVcpus: nonZeroInt(plan.MinVcpus), MaxVcpus: nonZeroInt(plan.MaxVcpus),
		MinMemoryGb: nonZeroInt(plan.MinMemoryGb), MaxMemoryGb: nonZeroInt(plan.MaxMemoryGb),
		// Null knobs are never sent: the key must be absent for the platform
		// default to apply (and to stay tracked).
		DrainTimeoutSeconds:         knownInt(plan.DrainTimeoutSeconds),
		ScaleDownDelaySeconds:       knownInt(plan.ScaleDownDelaySeconds),
		ScaleDownUtilizationPercent: knownInt(plan.ScaleDownUtilizationPercent),
	}
	if !plan.NodeLabels.IsNull() && !plan.NodeLabels.IsUnknown() {
		labels := map[string]string{}
		resp.Diagnostics.Append(plan.NodeLabels.ElementsAs(ctx, &labels, false)...)
		body.NodeLabels = &labels
	}
	if !plan.NodeTaints.IsNull() && !plan.NodeTaints.IsUnknown() {
		var tm []nodeTaintModel
		resp.Diagnostics.Append(plan.NodeTaints.ElementsAs(ctx, &tm, false)...)
		taints := make([]client.NodeTaint, 0, len(tm))
		for _, t := range tm {
			v := t.Value.ValueString()
			taints = append(taints, client.NodeTaint{Key: t.Key.ValueString(), Value: &v, Effect: client.NodeTaintEffect(t.Effect.ValueString())})
		}
		body.NodeTaints = &taints
	}
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.pd.Client.CreateNodePoolWithResponse(ctx, r.pd.orgID, clusterID, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create node pool", err.Error())
		return
	}
	// createNodePool answers 202 only, and JSON202 is the only typed success
	// field; a 200/201 would carry a body the parser never populated.
	if res.StatusCode() != http.StatusAccepted {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	// An id must never come from an unchecked decode: a 202 whose body is not
	// JSON leaves a ZERO UUID, and persisting it orphans the pool that was
	// really created. Failing here leaves no state entry at all.
	created := res.JSON202
	if created == nil {
		resp.Diagnostics.AddError("Aether API accepted the node pool but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+". The pool may exist; check the portal before retrying, since a retry with the same name is refused while it exists.")
		return
	}
	// Persist the id immediately, with the create response flattened in, so a
	// wait failure leaves an importable (tainted) resource in state.
	resp.Diagnostics.Append(r.flatten(ctx, created, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	final, err := r.waitForPool(ctx, clusterID, created.Id, createTimeout, false)
	if err != nil {
		resp.Diagnostics.AddError("Node pool did not become running", err.Error())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, final, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the pool; a 404 removes it from state so the next plan
// re-creates it.
func (r *nodePoolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state nodePoolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, poolID, ok := poolIDsFromState(&state, &resp.Diagnostics)
	if !ok {
		return
	}
	p, err := r.getPool(ctx, clusterID, poolID)
	if apierr.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		apierr.ToDiagnostics(err, &resp.Diagnostics, path.Empty())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, p, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// poolIDsFromState parses the cluster and pool ids held in state. State is
// data from outside this process, so a malformed id is an attribute
// diagnostic, not a request that cannot be built.
func poolIDsFromState(m *nodePoolModel, diags *diag.Diagnostics) (client.ClusterID, client.PoolID, bool) {
	clusterID, err := parseID(m.ClusterID.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("cluster_id"), "Invalid cluster_id in state", err.Error())
		return clusterID, client.PoolID{}, false
	}
	poolID, err := parseID(m.ID.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("id"), "Invalid node pool id in state", err.Error())
		return clusterID, poolID, false
	}
	return clusterID, poolID, true
}

// getPool reads one pool. A non-200 answer is returned as an *apierr.Error
// (so apierr.IsNotFound applies), and a 200 whose body is not JSON is an error
// too: it is not a reading of the pool.
func (r *nodePoolResource) getPool(ctx context.Context, clusterID client.ClusterID, poolID client.PoolID) (*client.NodePool, error) {
	g, err := r.pd.Client.GetNodePoolWithResponse(ctx, r.pd.orgID, clusterID, poolID)
	if err != nil {
		return nil, err
	}
	if g.StatusCode() != http.StatusOK {
		return nil, apierr.Parse(g.StatusCode(), g.HTTPResponse.Header, g.Body)
	}
	if g.JSON200 == nil {
		return nil, errors.New(describeNonJSON(g.HTTPResponse, g.Status()))
	}
	return g.JSON200, nil
}

// Update sends the changed node counts, budgets and knobs in one PUT. Only a
// node-count change on a FIXED pool is waited for; see the comments below for
// why nothing else can be.
func (r *nodePoolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state nodePoolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	updateTimeout, d := plan.Timeouts.Update(ctx, nodePoolUpdateTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, poolID, ok := poolIDsFromState(&state, &resp.Diagnostics)
	if !ok {
		return
	}
	// Read the pool first: a pool that is gone or unreadable fails here,
	// before anything is written, and this read is the whole of an update
	// that sends nothing.
	current, err := r.getPool(ctx, clusterID, poolID)
	if err != nil {
		apierr.ToDiagnostics(err, &resp.Diagnostics, path.Empty())
		return
	}
	// A PUT on a deleting pool answers 200 but is never applied (measured
	// contract): the reconciler never syncs a deleting pool's fields, so the
	// change would silently go nowhere. Refuse here instead of reporting a
	// success that never happens - this is also what makes the pre-read above
	// pay for itself on the non-waiting path.
	if string(current.Status) == poolStatusDeleting {
		resp.Diagnostics.AddError("Cannot update node pool",
			fmt.Sprintf("node pool %q is being deleted; the API accepts a PUT on it but never applies it.", state.Name.ValueString()))
		return
	}

	// name, sizes, labels, taints and cluster_id force replacement and never
	// reach Update, and are never put in the body: labels/taints are a 400,
	// and a rename or size change would be accepted by the API and then
	// misapplied by the platform (see Schema).
	var body client.UpdateNodePoolJSONRequestBody
	sent := false
	// Node counts travel TOGETHER. The API validates a partial count against
	// the STORED bounds but writes min/max from the request's desired_nodes:
	// a desired-only PUT is a 422 on a stored 1/1/1 pool and silently
	// collapses a stored 1/1/5 pool to 3/3/3 (aether-api@8e9045a store
	// nodepool.go:242-243).
	countsChanged := !plan.MinNodes.Equal(state.MinNodes) || !plan.DesiredNodes.Equal(state.DesiredNodes) || !plan.MaxNodes.Equal(state.MaxNodes)
	if countsChanged {
		body.MinNodes, body.DesiredNodes, body.MaxNodes = knownInt(plan.MinNodes), knownInt(plan.DesiredNodes), knownInt(plan.MaxNodes)
		sent = true
	}
	// Budgets are sent per field and EXPLICITLY, 0 included: an omitted
	// field keeps the stored value, so 0 is the only way back to a fixed pool.
	for _, f := range []struct {
		plan, state types.Int64
		dst         **int
	}{
		{plan.MinVcpus, state.MinVcpus, &body.MinVcpus},
		{plan.MaxVcpus, state.MaxVcpus, &body.MaxVcpus},
		{plan.MinMemoryGb, state.MinMemoryGb, &body.MinMemoryGb},
		{plan.MaxMemoryGb, state.MaxMemoryGb, &body.MaxMemoryGb},
		// Knobs are sent only when set and changed: null is never sent (the
		// API would keep the stored value anyway, and a removal is either
		// refused at plan or planned as the stored value).
		{plan.DrainTimeoutSeconds, state.DrainTimeoutSeconds, &body.DrainTimeoutSeconds},
		{plan.ScaleDownDelaySeconds, state.ScaleDownDelaySeconds, &body.ScaleDownDelaySeconds},
		{plan.ScaleDownUtilizationPercent, state.ScaleDownUtilizationPercent, &body.ScaleDownUtilizationPercent},
	} {
		if v := knownInt(f.plan); v != nil && !f.plan.Equal(f.state) {
			*f.dst = v
			sent = true
		}
	}

	plan.ID = state.ID
	if !sent {
		// Nothing to send (a timeouts-only edit, or labels/taints moving
		// between null and empty): record the read taken above.
		resp.Diagnostics.Append(r.flatten(ctx, current, &plan)...)
		warnIfDegraded(current, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	res, err := r.pd.Client.UpdateNodePoolWithResponse(ctx, r.pd.orgID, clusterID, poolID, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot update node pool", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}

	// Whether the pool autoscales is decided by the budget it has AFTER this
	// PUT, which is the planned budget (Default 0 keeps it always known).
	autoscaling := plan.MaxVcpus.ValueInt64() > plan.MinVcpus.ValueInt64() || plan.MaxMemoryGb.ValueInt64() > plan.MinMemoryGb.ValueInt64()
	if countsChanged && !autoscaling {
		// A PUT never changes status: straight after it the pool still reads
		// `running` with the OLD current_nodes, so the wait converges on
		// current_nodes == desired_nodes, not on the status alone.
		final, err := r.waitForPool(ctx, clusterID, poolID, updateTimeout, true)
		if err != nil {
			resp.Diagnostics.AddError("Node pool did not finish scaling", err.Error())
			return
		}
		resp.Diagnostics.Append(r.flatten(ctx, final, &plan)...)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	// Everything else - budgets, knobs, node counts on an autoscaling pool
	// (inert after create) - has NO convergence signal in the API: no
	// generation, no status change. Read once and return.
	after, err := r.getPool(ctx, clusterID, poolID)
	if err != nil {
		apierr.ToDiagnostics(err, &resp.Diagnostics, path.Empty())
		return
	}
	resp.Diagnostics.Append(r.flatten(ctx, after, &plan)...)
	warnIfDegraded(after, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// warnIfDegraded raises a warning for a pool read as degraded by an update
// that does not wait: the update succeeded, but the pool must not look healthy.
func warnIfDegraded(p *client.NodePool, diags *diag.Diagnostics) {
	if string(p.Status) == poolStatusDegraded {
		diags.AddWarning("Node pool is degraded", deref(p.StatusMessage))
	}
}

// Delete deletes the pool and waits until the API answers 404 for it.
func (r *nodePoolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state nodePoolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteTimeout, d := state.Timeouts.Delete(ctx, nodePoolDeleteTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, poolID, ok := poolIDsFromState(&state, &resp.Diagnostics)
	if !ok {
		return
	}
	res, err := r.pd.Client.DeleteNodePoolWithResponse(ctx, r.pd.orgID, clusterID, poolID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete node pool", err.Error())
		return
	}
	// 202 is the documented answer; 404 means it is already gone (a second
	// DELETE of a pool still `deleting` is a 404 too), which is what Delete
	// wants - the wait below confirms it.
	if code := res.StatusCode(); code != http.StatusNotFound && (code < 200 || code >= 300) {
		addAPIError(&resp.Diagnostics, code, res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	err = wait.ForGone(ctx, func(ctx context.Context) (bool, error) {
		g, err := r.pd.Client.GetNodePoolWithResponse(ctx, r.pd.orgID, clusterID, poolID)
		if err != nil {
			return false, err
		}
		// Only 404 means gone. A 2xx - including 200 `status: deleting`, which
		// the API answers until its sweep retires the row - means still
		// present. Anything else (a 403 after a token rotation, a 500) is
		// surfaced on the probe that saw it, not after the full timeout.
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
		resp.Diagnostics.AddError("Node pool deletion did not complete", err.Error())
	}
}

// ImportState imports a pool by `<cluster_id>/<pool_id>`; Read fills in the
// rest. Both parts are validated as UUIDs here, so a malformed id never
// reaches the API.
func (r *nodePoolResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Expected <cluster_id>/<pool_id> (two UUIDs separated by a slash), got %q.", req.ID))
		return
	}
	clusterID, err := parseID(parts[0])
	if err != nil {
		resp.Diagnostics.AddError("Invalid import id", "The cluster_id part of <cluster_id>/<pool_id> is not a UUID: "+err.Error())
		return
	}
	poolID, err := parseID(parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Invalid import id", "The pool_id part of <cluster_id>/<pool_id> is not a UUID: "+err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_id"), clusterID.String())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), poolID.String())...)
}

// waitForPool polls the pool until it is running and returns the last read.
//
// `degraded` is NOT terminal for a pool: the API debounces it (~15 s of an
// unavailable machine set) and clears it on the next healthy tick, so the
// wait polls through it. There is no failed status; `deleting` is treated as
// failed because a pool being deleted will never become running.
//
// converge is set after a node-count PUT on a fixed pool. The PUT leaves the
// status untouched (the API's reconciler moves it on a later tick), so a
// `running` read whose current_nodes != desired_nodes has not converged yet
// and is reported to the wait as a synthetic, non-terminal status.
//
// wait.ForStatus's timeout error quotes only the last STATUS, so a degraded
// read carries its message inside the status string: a pool that never
// leaves degraded must fail with the reason, not just the word.
func (r *nodePoolResource) waitForPool(ctx context.Context, clusterID client.ClusterID, poolID client.PoolID, timeout time.Duration, converge bool) (*client.NodePool, error) {
	var last *client.NodePool
	_, err := wait.ForStatus(ctx, func(ctx context.Context) (string, string, error) {
		p, err := r.getPool(ctx, clusterID, poolID)
		if err != nil {
			return "", "", err
		}
		last = p
		status, msg := string(p.Status), deref(p.StatusMessage)
		switch {
		case status == poolStatusDegraded && msg != "":
			return "degraded: " + msg, msg, nil
		case converge && status == poolStatusRunning && p.CurrentNodes != p.DesiredNodes:
			return fmt.Sprintf("running, %d of %d desired nodes ready", p.CurrentNodes, p.DesiredNodes), msg, nil
		}
		return status, msg, nil
	}, wait.Options{Timeout: timeout, Success: []string{poolStatusRunning}, Failed: []string{poolStatusDeleting}})
	if err != nil {
		return nil, err
	}
	return last, nil
}

// flatten copies an API pool into the model. m holds the planned (or prior)
// value, which decides how the API's "empty" spellings are recorded: `{}`
// labels and `[]` taints become null when m has null, and stay empty when the
// configuration spelled them empty. Timeouts is left untouched.
func (r *nodePoolResource) flatten(ctx context.Context, p *client.NodePool, m *nodePoolModel) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(p.Id.String())
	m.ClusterID = types.StringValue(p.ClusterId.String())
	m.Name = types.StringValue(p.Name)
	m.Vcpus = types.Int64Value(int64(p.Vcpus))
	m.MemoryGb = types.Int64Value(int64(p.MemoryGb))
	m.DiskGb = types.Int64Value(int64(p.DiskGb))
	m.DesiredNodes = types.Int64Value(int64(p.DesiredNodes))
	m.MinNodes = types.Int64Value(int64(p.MinNodes))
	m.MaxNodes = types.Int64Value(int64(p.MaxNodes))
	// The API always sends the four budgets (0 for none); an absent one is
	// the column default, 0, not null - the attributes default to 0.
	m.MinVcpus = types.Int64Value(int64(derefInt(p.MinVcpus)))
	m.MaxVcpus = types.Int64Value(int64(derefInt(p.MaxVcpus)))
	m.MinMemoryGb = types.Int64Value(int64(derefInt(p.MinMemoryGb)))
	m.MaxMemoryGb = types.Int64Value(int64(derefInt(p.MaxMemoryGb)))

	labels := map[string]string{}
	if p.NodeLabels != nil {
		labels = *p.NodeLabels
	}
	if !(len(labels) == 0 && m.NodeLabels.IsNull()) {
		v, d := types.MapValueFrom(ctx, types.StringType, labels)
		diags.Append(d...)
		m.NodeLabels = v
	}
	var taints []nodeTaintModel
	if p.NodeTaints != nil {
		for _, t := range *p.NodeTaints {
			// "value" is always echoed ("" when omitted); an absent one is
			// the same empty value.
			taints = append(taints, nodeTaintModel{Key: types.StringValue(t.Key), Value: types.StringValue(deref(t.Value)), Effect: types.StringValue(string(t.Effect))})
		}
	}
	if !(len(taints) == 0 && m.NodeTaints.IsNull()) {
		if taints == nil {
			taints = []nodeTaintModel{}
		}
		v, d := types.ListValueFrom(ctx, nodeTaintType, taints)
		diags.Append(d...)
		m.NodeTaints = v
	}

	// Absent means unset (tracking the platform default, which is never
	// echoed): null, never 0.
	m.DrainTimeoutSeconds = optInt(p.DrainTimeoutSeconds)
	m.ScaleDownDelaySeconds = optInt(p.ScaleDownDelaySeconds)
	m.ScaleDownUtilizationPercent = optInt(p.ScaleDownUtilizationPercent)
	m.CurrentNodes = types.Int64Value(int64(p.CurrentNodes))
	m.Status = types.StringValue(string(p.Status))
	m.StatusMessage = optString(p.StatusMessage)
	m.FirstReadyAt = types.StringNull()
	if p.FirstReadyAt != nil {
		m.FirstReadyAt = types.StringValue(p.FirstReadyAt.Format(time.RFC3339))
	}
	m.CreatedAt = types.StringValue(p.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(p.UpdatedAt.Format(time.RFC3339))
	return diags
}

// knownInt is the request form of an Int64 attribute: a pointer for a known
// value, nil (omitted from the JSON) for null or unknown.
func knownInt(v types.Int64) *int {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	i := int(v.ValueInt64())
	return &i
}

// nonZeroInt is knownInt that also omits 0, for fields where the API reads
// an omitted value and 0 identically.
func nonZeroInt(v types.Int64) *int {
	if p := knownInt(v); p != nil && *p != 0 {
		return p
	}
	return nil
}

// derefInt returns the int p points at, or 0 for nil.
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// optInt maps an optional API integer onto a Terraform value: absent (nil)
// becomes null, never 0.
func optInt(p *int) types.Int64 {
	if p == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*p))
}
