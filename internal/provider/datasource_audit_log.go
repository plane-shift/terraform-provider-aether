package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &auditLogDataSource{}

// NewAuditLogDataSource is aether_audit_log (T14-8).
func NewAuditLogDataSource() datasource.DataSource { return &auditLogDataSource{} }

type auditLogDataSource struct{ pd *providerData }

// auditLogDefaultLimit is the API's own default page size.
const auditLogDefaultLimit = 50

type auditEventDataModel struct {
	ID           types.String `tfsdk:"id"`
	ActorID      types.String `tfsdk:"actor_id"`
	ActorType    types.String `tfsdk:"actor_type"`
	ActorEmail   types.String `tfsdk:"actor_email"`
	Action       types.String `tfsdk:"action"`
	ResourceType types.String `tfsdk:"resource_type"`
	ResourceID   types.String `tfsdk:"resource_id"`
	MetadataJSON types.String `tfsdk:"metadata_json"`
	IPAddress    types.String `tfsdk:"ip_address"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

type auditLogModel struct {
	Limit  types.Int64           `tfsdk:"limit"`
	Events []auditEventDataModel `tfsdk:"events"`
}

// auditEventWire is one event as the API sends it, decoded here rather than
// through the generated AuditEvent so `metadata` (arbitrary JSON) keeps its
// exact text - numbers included - instead of round-tripping through
// interface{} and float64.
type auditEventWire struct {
	ID           string          `json:"id"`
	ActorID      *string         `json:"actor_id"`
	ActorType    *string         `json:"actor_type"`
	ActorEmail   *string         `json:"actor_email"`
	Action       *string         `json:"action"`
	ResourceType *string         `json:"resource_type"`
	ResourceID   *string         `json:"resource_id"`
	Metadata     json.RawMessage `json:"metadata"`
	IPAddress    *string         `json:"ip_address"`
	CreatedAt    *time.Time      `json:"created_at"`
}

func (d *auditLogDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_audit_log"
}

func (d *auditLogDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		// T14-8: verbatim.
		MarkdownDescription: "Returns only the newest `limit` events. The platform's cursor pagination beyond the first page is unreliable today (aether-api issue), so this data source does not page. `ip_address` is taken from the client's `X-Forwarded-For` header and is not authoritative.",
		Attributes: map[string]schema.Attribute{
			"limit": schema.Int64Attribute{Optional: true, Computed: true,
				MarkdownDescription: "Number of newest events to return, 1 to 100. Defaults to 50.",
				Validators:          []validator.Int64{int64validator.Between(1, 100)}},
			"events": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The newest events, newest first.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "Event UUID."},
					"actor_id":      schema.StringAttribute{Computed: true, MarkdownDescription: "UUID of the actor; null when none was recorded."},
					"actor_type":    schema.StringAttribute{Computed: true, MarkdownDescription: "`user`, `api_token` or `system`."},
					"actor_email":   schema.StringAttribute{Computed: true, MarkdownDescription: "Email of a `user` actor who is still a member; null otherwise."},
					"action":        schema.StringAttribute{Computed: true, MarkdownDescription: "What happened (free text, e.g. `registry.disable`)."},
					"resource_type": schema.StringAttribute{Computed: true, MarkdownDescription: "Kind of resource acted on (free text)."},
					"resource_id":   schema.StringAttribute{Computed: true, MarkdownDescription: "UUID of the resource acted on; null when none was recorded."},
					"metadata_json": schema.StringAttribute{Computed: true, MarkdownDescription: "Event metadata as compact JSON text (use `jsondecode`); null when the event has none."},
					"ip_address":    schema.StringAttribute{Computed: true, MarkdownDescription: "Client address from `X-Forwarded-For`; not authoritative. Null when none was recorded."},
					"created_at":    schema.StringAttribute{Computed: true, MarkdownDescription: "Event time (RFC 3339)."},
				}}},
		},
	}
}

func (d *auditLogDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *auditLogDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg auditLogModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	limit := auditLogDefaultLimit
	if !cfg.Limit.IsNull() && !cfg.Limit.IsUnknown() {
		limit = int(cfg.Limit.ValueInt64())
	}
	// ONE page only (T14-8): the API's cursor (`id <` over random UUIDs)
	// returns a wrong set beyond page 1, so next_cursor is deliberately
	// ignored.
	hr, err := d.pd.Client.ListAuditLog(ctx, d.pd.orgID, &client.ListAuditLogParams{Limit: &limit})
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the audit log", err.Error())
		return
	}
	body, err := readRawBody(hr)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the audit log", err.Error())
		return
	}
	if hr.StatusCode != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(hr.StatusCode, hr.Header, body), &resp.Diagnostics, path.Empty())
		return
	}
	var page struct {
		Items *[]auditEventWire `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil || page.Items == nil {
		resp.Diagnostics.AddError("Aether API returned no usable audit log", describeUndecodable(hr, hr.Status, err, "items"))
		return
	}
	out := auditLogModel{Limit: types.Int64Value(int64(limit)), Events: make([]auditEventDataModel, 0, len(*page.Items))}
	for _, e := range *page.Items {
		if e.ID == "" || e.CreatedAt == nil {
			resp.Diagnostics.AddError("Aether API returned no usable audit log", describeJSONWithout(hr, hr.Status, "event id or created_at"))
			return
		}
		for _, f := range []struct {
			name string
			val  *string
		}{
			{"actor_type", e.ActorType},
			{"action", e.Action},
			{"resource_type", e.ResourceType},
		} {
			if f.val == nil {
				resp.Diagnostics.AddError("Aether API returned an incomplete audit event",
					fmt.Sprintf("Event %s: %s.", e.ID, describeJSONWithout(hr, hr.Status, fmt.Sprintf("missing required field %q", f.name))))
				return
			}
		}
		md, err := compactMetadata(e.Metadata)
		if err != nil {
			resp.Diagnostics.AddError("Aether API returned an unreadable audit event",
				fmt.Sprintf("Event %s carries metadata that is not valid JSON: %s.", e.ID, err))
			return
		}
		out.Events = append(out.Events, auditEventDataModel{
			ID:           types.StringValue(e.ID),
			ActorID:      optString(e.ActorID),
			ActorType:    types.StringValue(*e.ActorType),
			ActorEmail:   optString(e.ActorEmail),
			Action:       types.StringValue(*e.Action),
			ResourceType: types.StringValue(*e.ResourceType),
			ResourceID:   optString(e.ResourceID),
			MetadataJSON: md,
			IPAddress:    optString(e.IPAddress),
			CreatedAt:    types.StringValue(e.CreatedAt.Format(time.RFC3339)),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

// compactMetadata renders event metadata as compact JSON text; absent or a
// JSON null reads as null.
func compactMetadata(raw json.RawMessage) (types.String, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return types.StringNull(), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return types.StringNull(), err
	}
	return types.StringValue(buf.String()), nil
}
