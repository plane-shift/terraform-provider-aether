package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
)

var (
	_ resource.Resource                = &webhookResource{}
	_ resource.ResourceWithConfigure   = &webhookResource{}
	_ resource.ResourceWithImportState = &webhookResource{}
	_ resource.ResourceWithModifyPlan  = &webhookResource{}
)

// NewWebhookResource returns the aether_webhook resource.
func NewWebhookResource() resource.Resource { return &webhookResource{} }

// webhookResource manages one outbound webhook of the provider's
// organization. Every operation is a single request except a create with
// enabled=false (POST, then PUT) and an update that also rotates the secret
// (PUT, then rotate). Nothing is waited for and there is no timeouts block
// (T10-11).
type webhookResource struct{ pd *providerData }

// webhookModel is the aether_webhook state; tfsdk tags are the attribute names.
type webhookModel struct {
	ID           types.String `tfsdk:"id"`
	URL          types.String `tfsdk:"url"`
	Description  types.String `tfsdk:"description"`
	Events       types.Set    `tfsdk:"events"`
	Enabled      types.Bool   `tfsdk:"enabled"`
	Secret       types.String `tfsdk:"secret"`
	RotateSecret types.String `tfsdk:"rotate_secret"`
	CreatedAt    types.String `tfsdk:"created_at"`
	UpdatedAt    types.String `tfsdk:"updated_at"`
}

// Metadata names the resource aether_webhook.
func (r *webhookResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_webhook"
}

// Schema defines aether_webhook.
func (r *webhookResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An outbound webhook of the provider's organization: the platform POSTs signed event payloads to `url`. An organization can hold at most 10 webhooks, disabled ones included; the 11th is refused by the API. " +
			"The signing `secret` is returned by the API only when the webhook is created and when it is rotated; it is never readable afterwards. Change `rotate_secret` to rotate it.",
		Attributes: map[string]schema.Attribute{
			"id": immutableComputed("Webhook UUID."),
			// Stored verbatim by the API, so no semantic equality is needed.
			"url": schema.StringAttribute{Required: true,
				MarkdownDescription: "Delivery endpoint. Must be an absolute `https` URL with a host and without embedded credentials. " +
					"The API also refuses literal IP addresses in private or internal ranges and, unless the platform allows it, `http`; those are checked at apply, not at plan.",
				Validators: []validator.String{webhookURLValidator{}}},
			// Optional+Computed with a "" default: removing the attribute plans
			// "" and sends "", which is the API's only way to clear it (a PUT
			// that omits the key keeps the stored value).
			"description": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""),
				MarkdownDescription: "Free-text description. Defaults to `\"\"`; removing it clears the stored description."},
			// A Set: the API keeps order and duplicates, but neither means
			// anything for delivery. The catalog is deliberately NOT a
			// validator (no route exposes it; the API's 422 names a bad type).
			"events": schema.SetAttribute{Required: true, ElementType: types.StringType,
				MarkdownDescription: "Event types to deliver, or exactly `[\"*\"]` for all of them. Known types (as of this provider's API version): " +
					"`cluster.created`, `cluster.ready`, `cluster.upgraded`, `cluster.degraded`, `cluster.deleted`, `node.replaced`, `usage.alert`, `invoice.generated`, `ticket.updated`. " +
					"An unknown type is refused by the API at apply.",
				Validators: []validator.Set{
					setvalidator.SizeAtLeast(1),
					setvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1)),
					webhookWildcardValidator{},
				}},
			"enabled": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				MarkdownDescription: "Whether events are delivered. Defaults to `true`. The API can only create a webhook enabled, so `enabled = false` at creation is a create followed by a disable call: " +
					"the endpoint may receive events between creation and the disable call (aether-api#306). If the disable call fails, the webhook is recorded enabled and marked tainted, and the next apply replaces it."},
			"secret": schema.StringAttribute{Computed: true, Sensitive: true,
				MarkdownDescription: "Signing secret (`whsec_` + 48 hex characters). The API returns it only on creation and on rotation, so it is null after an import until `rotate_secret` is set.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"rotate_secret": schema.StringAttribute{Optional: true,
				MarkdownDescription: "Change this value to rotate the signing secret; the value itself is never sent to the API. After an import, set it once to obtain a secret. " +
					"Setting it on a new webhook rotates nothing (creation already issues a fresh secret), and removing it rotates nothing."},
			"created_at": immutableComputed("Creation time (RFC 3339)."),
			// No UseStateForUnknown: the API moves it on EVERY PUT and rotate.
			"updated_at": observedComputed("Time the webhook was last written (RFC 3339)."),
		},
	}
}

// webhookURLValidator checks the url at plan time, never more strictly than
// the API: an absolute URL with scheme https OR http (a platform may allow
// http; the API decides), a non-empty host and no userinfo. Blocked IP ranges
// are left to the API's 422.
type webhookURLValidator struct{}

func (webhookURLValidator) Description(context.Context) string {
	return "must be an absolute http(s) URL with a host and without credentials"
}

func (v webhookURLValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (webhookURLValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	u, err := url.Parse(s)
	if err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid url", fmt.Sprintf("%q is not a valid URL: %s.", s, err))
		return
	}
	// url.Parse lower-cases the scheme.
	if u.Scheme != "https" && u.Scheme != "http" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid url",
			fmt.Sprintf("%q must be an absolute URL using the https scheme (http only where the platform allows it).", s))
		return
	}
	if u.Hostname() == "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid url", fmt.Sprintf("%q has no host.", s))
	}
	if u.User != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid url", fmt.Sprintf("%q must not contain credentials.", s))
	}
}

// webhookWildcardValidator refuses "*" alongside any other entry, which the
// API answers with a 422.
type webhookWildcardValidator struct{}

func (webhookWildcardValidator) Description(context.Context) string {
	return `the wildcard "*" must be the only entry`
}

func (v webhookWildcardValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (webhookWildcardValidator) ValidateSet(_ context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	elems := req.ConfigValue.Elements()
	if len(elems) < 2 {
		return
	}
	for _, e := range elems {
		if s, ok := e.(types.String); ok && !s.IsNull() && !s.IsUnknown() && s.ValueString() == "*" {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid events", `the wildcard "*" must be the only entry`)
			return
		}
	}
}

// Configure receives the provider's client and organization.
func (r *webhookResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan handles the two attributes the framework cannot plan on its own
// for an update. When the rotate_secret trigger fires (config non-null and
// different from state, a null state counting as different) the secret will
// change, so it is planned unknown (otherwise UseStateForUnknown carries the
// prior value, null included). updated_at moves on every PUT and every
// rotate, so any update plans it unknown.
func (r *webhookResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy: Create never rotates
	}
	var state, plan webhookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if rotateTriggered(plan.RotateSecret, state.RotateSecret) {
		// The attribute-level UseStateForUnknown already keeps `secret` known
		// (null included) on every non-rotating update; only a rotation
		// overrides it with unknown.
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("secret"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("updated_at"), types.StringUnknown())...)
		return
	}
	if !req.Plan.Raw.Equal(req.State.Raw) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("updated_at"), types.StringUnknown())...)
	}
}

// rotateTriggered is the one rule for "this update rotates the secret": the
// planned rotate_secret is set and differs from the prior one. An unknown
// planned value counts as a change (it is decided again in Update, where it
// is known). Removing the value (null) never rotates.
func rotateTriggered(planned, prior types.String) bool {
	if planned.IsNull() {
		return false
	}
	return planned.IsUnknown() || !planned.Equal(prior)
}

// Create POSTs exactly {url, description, events} - the API refuses
// `enabled` in the body, so a webhook is always created enabled - and then,
// for enabled=false, disables it with a PUT of exactly {"enabled":false}.
// rotate_secret is only stored: creation already issued a fresh secret.
func (r *webhookResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan webhookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	events, d := sortedEvents(ctx, plan.Events)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	desc := plan.Description.ValueString()
	body := client.CreateWebhookRequest{Url: plan.URL.ValueString(), Description: &desc, Events: events}
	res, err := r.pd.Client.CreateWebhookWithResponse(ctx, r.pd.orgID, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create webhook", err.Error())
		return
	}
	if res.StatusCode() != http.StatusCreated {
		addAPIError(&resp.Diagnostics, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return
	}
	if res.JSON201 == nil {
		resp.Diagnostics.AddError("Aether API accepted the webhook but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+". The webhook was probably created, but it is not recorded in state and its secret is lost: delete it through the API, or import it by its id and set `rotate_secret` to obtain a secret.")
		return
	}
	// Read before flattening: the 201 always says enabled=true.
	wantDisabled := !plan.Enabled.ValueBool()
	resp.Diagnostics.Append(flattenWebhook(ctx, res.JSON201, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if res.JSON201.Secret == nil {
		// Unreachable against the measured API (every 201 carries the
		// secret), but the id is in hand: SAVE the webhook with a null secret
		// and fail, so Terraform taints it and the next apply replaces it
		// instead of orphaning it against the per-organization cap (review
		// r1 M-2, same shape as the failed-disable path below). The disable
		// call is not attempted: the replacement makes it moot.
		plan.Secret = types.StringNull()
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		resp.Diagnostics.AddError("Aether API returned the webhook without a signing secret",
			"The webhook was created, but the API's answer carried no signing secret. It is saved in state and marked tainted: the next apply replaces it.")
		return
	}
	plan.Secret = types.StringValue(*res.JSON201.Secret)
	if wantDisabled {
		// The planned value was false and the 201 says true. If the disable
		// fails, state from the 201 is SAVED with the error (T10-3): the secret
		// is visible only now, so the webhook must not be lost; Terraform
		// taints it and the next apply replaces it.
		off := false
		disabled, diags := r.put(ctx, plan.ID.ValueString(), client.UpdateWebhookRequest{Enabled: &off})
		if diags.HasError() {
			resp.Diagnostics.Append(diags...)
			resp.Diagnostics.AddError("Webhook created but not disabled",
				"The webhook was created ENABLED and its disable call failed, so it may be receiving events. It is recorded in state as enabled and tainted: the next apply replaces it.")
			resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
			return
		}
		resp.Diagnostics.Append(flattenWebhook(ctx, disabled, &plan)...)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the webhook. The API never returns the secret on GET, so
// `secret` (and `rotate_secret`, which exists only in Terraform) are carried
// from prior state. 404 removes the resource from state; every other failure,
// a 403 included, is an error and keeps state.
func (r *webhookResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state webhookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid id in state", err.Error())
		return
	}
	g, err := r.pd.Client.GetWebhookWithResponse(ctx, r.pd.orgID, id)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read webhook", err.Error())
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
		resp.Diagnostics.AddError("Aether API returned no usable webhook", describeNonJSON(g.HTTPResponse, g.Status()))
		return
	}
	resp.Diagnostics.Append(flattenWebhook(ctx, g.JSON200, &state)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update sends a PUT carrying ONLY the changed fields among url, description,
// events and enabled (the API merges partially), then - when the
// rotate_secret trigger fired - rotates the secret. Either call is skipped
// when it has nothing to do. If the PUT succeeds and the rotation fails, the
// PUT's result is saved with the prior secret and the PRIOR rotate_secret, so
// the next plan triggers the rotation again.
func (r *webhookResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state webhookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Start from prior state; each successful call overwrites what it returns.
	next := state
	next.RotateSecret = plan.RotateSecret

	var body client.UpdateWebhookRequest
	changed := false
	if !plan.URL.Equal(state.URL) {
		body.Url, changed = plan.URL.ValueStringPointer(), true
	}
	if !plan.Description.Equal(state.Description) {
		body.Description, changed = plan.Description.ValueStringPointer(), true
	}
	if !plan.Events.Equal(state.Events) {
		events, d := sortedEvents(ctx, plan.Events)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		body.Events, changed = &events, true
	}
	if !plan.Enabled.Equal(state.Enabled) {
		body.Enabled, changed = plan.Enabled.ValueBoolPointer(), true
	}
	if changed {
		w, diags := r.put(ctx, state.ID.ValueString(), body)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return // nothing was written: prior state stands
		}
		resp.Diagnostics.Append(flattenWebhook(ctx, w, &next)...)
	}
	if rotateTriggered(plan.RotateSecret, state.RotateSecret) {
		w, diags := r.rotate(ctx, state.ID.ValueString())
		if diags.HasError() {
			resp.Diagnostics.Append(diags...)
			if changed {
				// Record what the PUT wrote; the old rotate_secret keeps the
				// rotation pending for the next plan.
				next.RotateSecret = state.RotateSecret
				resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
			}
			return
		}
		resp.Diagnostics.Append(flattenWebhook(ctx, w, &next)...)
		next.Secret = types.StringValue(*w.Secret)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

// Delete removes the webhook. 204 and 404 (already gone) are success.
func (r *webhookResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state webhookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("id"), "Invalid id in state", err.Error())
		return
	}
	res, err := r.pd.Client.DeleteWebhookWithResponse(ctx, r.pd.orgID, id)
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete webhook", err.Error())
		return
	}
	if code := res.StatusCode(); code != http.StatusNoContent && code != http.StatusNotFound {
		addAPIError(&resp.Diagnostics, code, res.HTTPResponse.Header, res.Body, path.Empty())
	}
}

// ImportState imports a webhook by its id (canonical UUID text); Read fills
// in the rest. The secret cannot be read back, so it stays null until
// rotate_secret is set.
func (r *webhookResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := parseID(req.ID)
	if err != nil || id.String() != req.ID {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Expected the webhook's id as a canonical lower-case UUID (36 characters), got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id.String())...)
}

// put sends one partial-merge PUT and returns the stored webhook (which never
// carries the secret).
func (r *webhookResource) put(ctx context.Context, rawID string, body client.UpdateWebhookRequest) (*client.Webhook, diag.Diagnostics) {
	var diags diag.Diagnostics
	id, err := parseID(rawID)
	if err != nil {
		diags.AddAttributeError(path.Root("id"), "Invalid webhook id", err.Error())
		return nil, diags
	}
	res, err := r.pd.Client.UpdateWebhookWithResponse(ctx, r.pd.orgID, id, body)
	if err != nil {
		diags.AddError("Cannot update webhook", err.Error())
		return nil, diags
	}
	if res.StatusCode() != http.StatusOK {
		addAPIError(&diags, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return nil, diags
	}
	if res.JSON200 == nil {
		diags.AddError("Aether API accepted the webhook update but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+". The update was probably applied, but state still holds the previous values: apply again (the update is idempotent).")
		return nil, diags
	}
	return res.JSON200, diags
}

// rotate issues a new secret and returns the webhook carrying it.
func (r *webhookResource) rotate(ctx context.Context, rawID string) (*client.Webhook, diag.Diagnostics) {
	var diags diag.Diagnostics
	id, err := parseID(rawID)
	if err != nil {
		diags.AddAttributeError(path.Root("id"), "Invalid webhook id", err.Error())
		return nil, diags
	}
	res, err := r.pd.Client.RotateWebhookSecretWithResponse(ctx, r.pd.orgID, id)
	if err != nil {
		diags.AddError("Cannot rotate webhook secret", err.Error())
		return nil, diags
	}
	if res.StatusCode() != http.StatusOK {
		addAPIError(&diags, res.StatusCode(), res.HTTPResponse.Header, res.Body, path.Empty())
		return nil, diags
	}
	if res.JSON200 == nil || res.JSON200.Secret == nil {
		diags.AddError("Aether API rotated the webhook secret but returned no usable body",
			describeNonJSON(res.HTTPResponse, res.Status())+" (or no secret). The secret was probably rotated and the new one is lost; state keeps the old one: change `rotate_secret` again to obtain a new secret.")
		return nil, diags
	}
	return res.JSON200, diags
}

// sortedEvents returns the set's elements sorted, so request bodies are
// deterministic (the API stores events in the order sent).
func sortedEvents(ctx context.Context, set types.Set) ([]string, diag.Diagnostics) {
	events := []string{}
	diags := set.ElementsAs(ctx, &events, false)
	sort.Strings(events)
	return events, diags
}

// flattenWebhook copies an API webhook into the model. Events are
// de-duplicated (an out-of-band writer may store duplicates; the API never
// removes them). secret and rotate_secret are never touched: GET and PUT do
// not return the secret, so callers set it where the API did.
func flattenWebhook(ctx context.Context, w *client.Webhook, m *webhookModel) diag.Diagnostics {
	m.ID = types.StringValue(w.Id.String())
	m.URL = types.StringValue(w.Url)
	m.Description = types.StringValue(w.Description)
	seen := make(map[string]bool, len(w.Events))
	events := make([]string, 0, len(w.Events))
	for _, e := range w.Events {
		if !seen[e] {
			seen[e] = true
			events = append(events, e)
		}
	}
	set, diags := types.SetValueFrom(ctx, types.StringType, events)
	m.Events = set
	m.Enabled = types.BoolValue(w.Enabled)
	m.CreatedAt = types.StringValue(w.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(w.UpdatedAt.Format(time.RFC3339))
	return diags
}
