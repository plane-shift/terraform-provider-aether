package provider

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
)

// Default operation timeouts, overridable through the `timeouts` attribute.
// Every operation is ONE PUT and nothing is waited for (the API exposes no
// signal that the platform has applied a policy), so they bound that single
// request only and are short.
const (
	clusterNetworkCreateTimeout = 5 * time.Minute
	clusterNetworkUpdateTimeout = 5 * time.Minute
	clusterNetworkDeleteTimeout = 5 * time.Minute
)

var (
	_ resource.Resource                = &clusterNetworkResource{}
	_ resource.ResourceWithConfigure   = &clusterNetworkResource{}
	_ resource.ResourceWithImportState = &clusterNetworkResource{}
)

// NewClusterNetworkResource returns the aether_cluster_network resource.
func NewClusterNetworkResource() resource.Resource { return &clusterNetworkResource{} }

// clusterNetworkResource manages the network policy (control-plane source-IP
// allowlist and API-server rate limit) of one cluster. The policy is a
// singleton per cluster that ALWAYS exists as far as the API is concerned: a
// never-configured cluster reads as an empty list, and there is no DELETE.
type clusterNetworkResource struct{ pd *providerData }

// clusterNetworkModel is the aether_cluster_network state; tfsdk tags are the
// attribute names.
type clusterNetworkModel struct {
	ID                 types.String   `tfsdk:"id"`
	ClusterID          types.String   `tfsdk:"cluster_id"`
	AuthorizedCidrs    types.List     `tfsdk:"authorized_cidrs"`
	ApiServerRateLimit types.Object   `tfsdk:"api_server_rate_limit"`
	UpdatedAt          types.String   `tfsdk:"updated_at"`
	Timeouts           timeouts.Value `tfsdk:"timeouts"`
}

// rateLimitModel is the api_server_rate_limit block.
type rateLimitModel struct {
	MaxConnectionsPerSecond types.Int64 `tfsdk:"max_connections_per_second"`
	Burst                   types.Int64 `tfsdk:"burst"`
}

// rateLimitAttrTypes is the object type of api_server_rate_limit; it must
// match the nested attributes declared in Schema.
var rateLimitAttrTypes = map[string]attr.Type{
	"max_connections_per_second": types.Int64Type,
	"burst":                      types.Int64Type,
}

// Metadata names the resource aether_cluster_network.
func (r *clusterNetworkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster_network"
}

// rateLimitInt64 is one of the two rate-limit values. The platform stores
// them in 32-bit integer columns: the API refuses <= 0 with a 422 but answers
// a value above 2147483647 with a 500, so both ends are checked at plan time.
func rateLimitInt64(desc string) schema.Int64Attribute {
	return schema.Int64Attribute{Required: true, MarkdownDescription: desc,
		Validators: []validator.Int64{int64validator.Between(1, math.MaxInt32)}}
}

// Schema defines aether_cluster_network.
func (r *clusterNetworkResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The network policy of an `aether_cluster`: which source addresses may reach its public control-plane endpoint, and the API-server connection rate limit. There is exactly one policy per cluster. " +
			"Creating this resource OVERWRITES whatever policy the cluster has (for example one set in the portal) with the configuration; import to adopt an existing policy instead. " +
			"Destroying this resource OPENS the cluster's public endpoint to any source address and resets the API-server rate limit to the platform default. " +
			"The platform applies the policy asynchronously; this resource never waits for enforcement. " +
			"Do not set `create_before_destroy` on this resource: a replacement of the same cluster would run the destroy, which opens the endpoint, after the create.",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("Same as `cluster_id` (the policy has no id of its own)."),
			"cluster_id": schema.StringAttribute{Required: true,
				MarkdownDescription: "UUID of the `aether_cluster` the policy belongs to. Changing it replaces the resource, which first OPENS the old cluster's endpoint (see the resource description).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{canonicalUUIDValidator{}}},
			// A List, not a Set: the API stores and echoes the entries verbatim
			// and in order, so the configured order is the truth.
			"authorized_cidrs": schema.ListAttribute{Required: true, ElementType: types.StringType,
				MarkdownDescription: "Source-address allowlist in CIDR notation (e.g. `203.0.113.0/24`, `198.51.100.7/32`). An empty list (`[]`) accepts connections from ANY source address; authentication still applies. " +
					"Each entry must be an IPv4 network address in canonical form (no host bits set and no leading zeros, so `10.0.0.0/8`, not `10.0.0.1/8` or `10.0.0.0/08`) and appear only once: the API accepts the other spellings, IPv6 and duplicates, but the platform's cluster schema accepts only IPv4 network prefixes without duplicates, so the platform would keep enforcing the previous policy; those are refused at plan time (or at apply, for an entry not known until then).",
				Validators: []validator.List{cidrListValidator{}}},
			// Optional and NOT Computed: absent means "the platform default",
			// which is never sent and never echoed. Removing the block omits the
			// key from the PUT, which clears the stored value.
			"api_server_rate_limit": schema.SingleNestedAttribute{Optional: true,
				MarkdownDescription: "API-server connection rate limit. Omit it for the platform default; removing it resets the limit to that default. " +
					"The rate limit takes effect only when the cluster's control-plane proxy is (re)created; a running proxy keeps the limit it booted with.",
				Attributes: map[string]schema.Attribute{
					"max_connections_per_second": rateLimitInt64("New connections per second allowed (1-2147483647)."),
					"burst":                      rateLimitInt64("Connections allowed in a burst above the per-second rate (1-2147483647)."),
				}},
			// No UseStateForUnknown: the API moves it on EVERY PUT, so an update
			// must plan it unknown.
			"updated_at": observedComputed("Time the policy was last written (RFC 3339). A never-configured cluster reads `0001-01-01T00:00:00Z`."),
			"timeouts":   timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

// canonicalUUIDValidator requires cluster_id to be canonical lower-case UUID
// text. parseID also accepts upper case (and braces), but the API answers
// with the canonical form, so a non-canonical value would be written by the
// PUT and then fail with "inconsistent result after apply" - refused at plan
// time instead, before any request.
type canonicalUUIDValidator struct{}

func (canonicalUUIDValidator) Description(context.Context) string {
	return "must be a canonical lower-case UUID"
}

func (v canonicalUUIDValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (canonicalUUIDValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	if u, err := parseID(s); err != nil || u.String() != s {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid cluster_id",
			fmt.Sprintf("%q must be a canonical lower-case UUID (36 characters, lower-case hex and hyphens, e.g. the `id` of an `aether_cluster`).", s))
	}
}

// cidrProblem is one refused authorized_cidrs entry.
type cidrProblem struct {
	index           int
	summary, detail string
}

// checkCIDRs returns the authorized_cidrs entries the API accepts (200) but
// the platform does not: anything that is not canonical IPv4 CIDR notation
// for a network address, and duplicates. The platform's cluster schema
// accepts only IPv4 network prefixes without duplicates, so such a policy is
// expected to be rejected there while the API reports it stored, and the
// control-plane proxy keeps enforcing the OLD one - a silent divergence
// (measured contract §7.3). known[i] false skips entry i (unknown at plan
// time); it is checked again at apply, when it is known.
//
// This is the one check behind both the plan-time validator and the
// apply-time re-check in put().
func checkCIDRs(entries []string, known []bool) []cidrProblem {
	var out []cidrProblem
	seen := map[string]int{} // canonical network -> first index
	for i, entry := range entries {
		if !known[i] {
			continue
		}
		ip, ipnet, err := net.ParseCIDR(entry)
		if err != nil {
			out = append(out, cidrProblem{i, "Invalid CIDR",
				fmt.Sprintf("%q is not in CIDR notation (address/prefix-length, e.g. 203.0.113.0/24; a single address is written 198.51.100.7/32).", entry)})
			continue
		}
		// The mask length, not ip.To4(): an IPv4-mapped IPv6 prefix
		// (::ffff:10.0.0.0/104) has a 4-byte IP but is still an IPv6 network.
		if len(ipnet.Mask) != net.IPv4len {
			out = append(out, cidrProblem{i, "Only IPv4 CIDRs are supported",
				fmt.Sprintf("%q is not an IPv4 CIDR. The platform's cluster schema accepts IPv4 network prefixes only; an IPv6 entry would be stored by the API but leave the platform enforcing the previous policy.", entry)})
			continue
		}
		if !ip.Equal(ipnet.IP) {
			out = append(out, cidrProblem{i, "CIDR has host bits set",
				fmt.Sprintf("%q has host bits set; use its network address %q instead.", entry, ipnet.String())})
			continue
		}
		// ParseCIDR accepts spellings the platform's schema does not, such as
		// a leading-zero prefix length (10.0.0.0/08).
		if ipnet.String() != entry {
			out = append(out, cidrProblem{i, "CIDR is not in canonical form",
				fmt.Sprintf("%q is not in canonical form; write the network address as %q.", entry, ipnet.String())})
			continue
		}
		if first, dup := seen[entry]; dup {
			out = append(out, cidrProblem{i, "Duplicate CIDR",
				fmt.Sprintf("%q is a duplicate of entry %d; the platform's cluster schema accepts no duplicates and the platform would keep enforcing the previous policy.", entry, first)})
			continue
		}
		seen[entry] = i
	}
	return out
}

// cidrListValidator runs checkCIDRs at plan time, on the entries already known.
type cidrListValidator struct{}

func (cidrListValidator) Description(context.Context) string {
	return "each entry must be an IPv4 network address in canonical CIDR notation, and entries must be unique"
}

func (v cidrListValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (cidrListValidator) ValidateList(_ context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	elems := req.ConfigValue.Elements()
	entries, known := make([]string, len(elems)), make([]bool, len(elems))
	for i, e := range elems {
		if s, ok := e.(types.String); ok && !s.IsNull() && !s.IsUnknown() {
			entries[i], known[i] = s.ValueString(), true
		}
	}
	for _, p := range checkCIDRs(entries, known) {
		resp.Diagnostics.AddAttributeError(req.Path.AtListIndex(p.index), p.summary, p.detail)
	}
}

// Configure receives the provider's client and organization.
func (r *clusterNetworkResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// Create writes the configured policy with one PUT. It deliberately does not
// read first: the declared configuration replaces whatever is stored (import
// adopts instead), and there is no convergence signal to wait on.
func (r *clusterNetworkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clusterNetworkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := plan.Timeouts.Create(ctx, clusterNetworkCreateTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp.Diagnostics.Append(r.put(ctx, &plan, true)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the policy. A 404 means the CLUSTER is gone (a policy is
// never "not found" on an existing cluster), so the resource is removed from
// state; every other failure, a 403 included, is an error and keeps state.
func (r *clusterNetworkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state clusterNetworkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	clusterID, err := parseID(state.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("cluster_id"), "Invalid cluster_id in state", err.Error())
		return
	}
	g, err := r.pd.Client.GetClusterNetworkWithResponse(ctx, r.pd.orgID, clusterID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read cluster network policy", err.Error())
		return
	}
	switch {
	case g.StatusCode() == http.StatusNotFound:
		resp.State.RemoveResource(ctx)
		return
	case g.StatusCode() != http.StatusOK:
		addAPIError(&resp.Diagnostics, g.StatusCode(), g.HTTPResponse.Header, g.Body, path.Empty())
		return
	case g.JSON200 == nil:
		resp.Diagnostics.AddError("Aether API returned no usable cluster network policy", describeNonJSON(g.HTTPResponse, g.Status()))
		return
	}
	resp.Diagnostics.Append(flattenClusterNetwork(ctx, g.JSON200, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update replaces the stored policy with the planned one in one PUT (the API
// has no partial update: the whole list is always sent, and the rate-limit
// block is sent or omitted - omitted resets it). Nothing is waited for.
func (r *clusterNetworkResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan clusterNetworkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := plan.Timeouts.Update(ctx, clusterNetworkUpdateTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp.Diagnostics.Append(r.put(ctx, &plan, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete resets the policy to "open": one PUT of EXACTLY
// {"authorized_cidrs":[]}. The rate-limit key is omitted, which resets it to
// the platform default - a zero object would be a 422 and destroy could never
// succeed. A 404 (the cluster is already gone) is success.
func (r *clusterNetworkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterNetworkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, d := state.Timeouts.Delete(ctx, clusterNetworkDeleteTimeout)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	clusterID, err := parseID(state.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("cluster_id"), "Invalid cluster_id in state", err.Error())
		return
	}
	// A non-nil empty slice: the field has no omitempty and `null` is a 500.
	body := client.UpsertClusterNetworkJSONRequestBody{AuthorizedCidrs: []string{}}
	res, err := r.pd.Client.UpsertClusterNetworkWithResponse(ctx, r.pd.orgID, clusterID, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reset cluster network policy", err.Error())
		return
	}
	if code := res.StatusCode(); code != http.StatusOK && code != http.StatusNotFound {
		addAPIError(&resp.Diagnostics, code, res.HTTPResponse.Header, res.Body, path.Empty())
	}
}

// ImportState imports the policy of a cluster by the cluster's UUID; Read
// fills in the rest. Any existing cluster imports, including one never
// configured (it reads as an empty list and no rate limit).
func (r *clusterNetworkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	clusterID, err := parseID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import id", "Expected the cluster's UUID: "+err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_id"), clusterID.String())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), clusterID.String())...)
}

// put sends m as the whole policy and flattens the response into m. The
// response IS the stored row, so it is recorded as-is: no read follows,
// because a read could not show anything the PUT answer does not.
func (r *clusterNetworkResource) put(ctx context.Context, m *clusterNetworkModel, creating bool) diag.Diagnostics {
	var diags diag.Diagnostics
	clusterID, err := parseID(m.ClusterID.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("cluster_id"), "Invalid cluster_id", err.Error())
		return diags
	}
	// A non-nil slice even for `[]`: the field has no omitempty and the API
	// answers `null` with a 500.
	body := client.UpsertClusterNetworkJSONRequestBody{AuthorizedCidrs: []string{}}
	diags.Append(m.AuthorizedCidrs.ElementsAs(ctx, &body.AuthorizedCidrs, false)...)
	if body.AuthorizedCidrs == nil {
		body.AuthorizedCidrs = []string{}
	}
	// Config validators do not run again at apply, so an entry that was
	// unknown at plan time (built from another resource's output) has not
	// been checked yet: check the whole planned list before it is sent.
	if !diags.HasError() {
		known := make([]bool, len(body.AuthorizedCidrs))
		for i := range known {
			known[i] = true
		}
		for _, p := range checkCIDRs(body.AuthorizedCidrs, known) {
			diags.AddAttributeError(path.Root("authorized_cidrs").AtListIndex(p.index), p.summary, p.detail)
		}
	}
	if !m.ApiServerRateLimit.IsNull() && !m.ApiServerRateLimit.IsUnknown() {
		var rl rateLimitModel
		diags.Append(m.ApiServerRateLimit.As(ctx, &rl, basetypes.ObjectAsOptions{})...)
		body.ApiServerRateLimit = &client.APIServerRateLimit{
			MaxConnectionsPerSecond: int(rl.MaxConnectionsPerSecond.ValueInt64()),
			Burst:                   int(rl.Burst.ValueInt64()),
		}
	}
	if diags.HasError() {
		return diags
	}
	res, err := r.pd.Client.UpsertClusterNetworkWithResponse(ctx, r.pd.orgID, clusterID, body)
	if err != nil {
		summary := "Cannot update cluster network policy"
		if creating {
			summary = "Cannot set cluster network policy"
		}
		diags.AddError(summary, err.Error())
		return diags
	}
	if res.StatusCode() != http.StatusOK {
		addAPIError(&diags, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return diags
	}
	if res.JSON200 == nil {
		// On create nothing is saved to state, so a refresh would read nothing;
		// on update the prior state is kept and the next plan shows the diff.
		advice := " The policy was probably written, but it is not recorded in state: apply again (the PUT is idempotent), or import the cluster's policy to adopt it."
		if !creating {
			advice = " The policy was probably written, but state still holds the previous policy: apply again (the PUT is idempotent)."
		}
		diags.AddError("Aether API accepted the cluster network policy but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+"."+advice)
		return diags
	}
	diags.Append(flattenClusterNetwork(ctx, res.JSON200, m)...)
	return diags
}

// flattenClusterNetwork copies an API policy into the model. An absent rate
// limit is null (the platform default, never echoed); updated_at is recorded
// as text even when it is the zero time of a never-configured cluster.
// Timeouts is left untouched.
func flattenClusterNetwork(ctx context.Context, n *client.ClusterNetwork, m *clusterNetworkModel) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(n.ClusterId.String())
	m.ClusterID = types.StringValue(n.ClusterId.String())
	cidrs := n.AuthorizedCidrs
	if cidrs == nil {
		cidrs = []string{}
	}
	v, d := types.ListValueFrom(ctx, types.StringType, cidrs)
	diags.Append(d...)
	m.AuthorizedCidrs = v
	m.ApiServerRateLimit = types.ObjectNull(rateLimitAttrTypes)
	if rl := n.ApiServerRateLimit; rl != nil {
		o, d := types.ObjectValueFrom(ctx, rateLimitAttrTypes, rateLimitModel{
			MaxConnectionsPerSecond: types.Int64Value(int64(rl.MaxConnectionsPerSecond)),
			Burst:                   types.Int64Value(int64(rl.Burst)),
		})
		diags.Append(d...)
		m.ApiServerRateLimit = o
	}
	m.UpdatedAt = types.StringValue(n.UpdatedAt.Format(time.RFC3339))
	return diags
}
