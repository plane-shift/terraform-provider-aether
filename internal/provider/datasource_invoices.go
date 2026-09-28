package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &invoicesDataSource{}

// NewInvoicesDataSource is aether_invoices (T14-11).
func NewInvoicesDataSource() datasource.DataSource { return &invoicesDataSource{} }

type invoicesDataSource struct{ pd *providerData }

const invoiceListDSPageSize = 100

// invoiceDataModel exposes no buyer (customer PII), no issuer and no pdf_url
// (never written) - T14-11. Money is cents (Int64, T14-13).
type invoiceDataModel struct {
	ID            types.String `tfsdk:"id"`
	InvoiceNumber types.String `tfsdk:"invoice_number"`
	PeriodStart   types.String `tfsdk:"period_start"`
	PeriodEnd     types.String `tfsdk:"period_end"`
	SubtotalCents types.Int64  `tfsdk:"subtotal_cents"`
	TaxCents      types.Int64  `tfsdk:"tax_cents"`
	TotalCents    types.Int64  `tfsdk:"total_cents"`
	TaxRateBP     types.Int64  `tfsdk:"tax_rate_bp"`
	TaxNote       types.String `tfsdk:"tax_note"`
	Status        types.String `tfsdk:"status"`
	CreatedAt     types.String `tfsdk:"created_at"`
}

type invoicesModel struct {
	Invoices []invoiceDataModel `tfsdk:"invoices"`
}

// invoiceWire is the subset of an invoice the data source reads. The
// generated Invoice lacks invoice_number, tax_rate_bp and tax_note, so the
// body is decoded here; buyer/issuer/pdf_url are deliberately not declared,
// so they are never even decoded.
type invoiceWire struct {
	ID            string     `json:"id"`
	InvoiceNumber string     `json:"invoice_number"`
	PeriodStart   *time.Time `json:"period_start"`
	PeriodEnd     *time.Time `json:"period_end"`
	SubtotalCents int64      `json:"subtotal_cents"`
	TaxCents      int64      `json:"tax_cents"`
	TotalCents    int64      `json:"total_cents"`
	TaxRateBP     int64      `json:"tax_rate_bp"`
	TaxNote       string     `json:"tax_note"`
	Status        string     `json:"status"`
	CreatedAt     *time.Time `json:"created_at"`
}

func (d *invoicesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_invoices"
}

func (d *invoicesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The organization's invoices, newest period first. Amounts are integer cents. " +
			"The buyer and issuer details printed on an invoice are not exposed.",
		Attributes: map[string]schema.Attribute{
			"invoices": schema.ListNestedAttribute{Computed: true,
				MarkdownDescription: "The invoices, in the API's order (newest period first).",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "Invoice UUID."},
					"invoice_number": schema.StringAttribute{Computed: true, MarkdownDescription: "Customer-facing invoice number (`AE-YYYY-NNNN`)."},
					"period_start":   schema.StringAttribute{Computed: true, MarkdownDescription: "First day of the invoiced period (RFC 3339)."},
					"period_end":     schema.StringAttribute{Computed: true, MarkdownDescription: "End of the invoiced period (RFC 3339)."},
					"subtotal_cents": schema.Int64Attribute{Computed: true, MarkdownDescription: "Net amount in euro cents."},
					"tax_cents":      schema.Int64Attribute{Computed: true, MarkdownDescription: "Tax in euro cents."},
					"total_cents":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Gross amount in euro cents."},
					"tax_rate_bp":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Tax rate in basis points (1900 = 19%)."},
					"tax_note":       schema.StringAttribute{Computed: true, MarkdownDescription: "Tax note printed on the invoice (e.g. reverse charge)."},
					"status":         schema.StringAttribute{Computed: true, MarkdownDescription: "`draft`, `issued`, `paid` or `void`."},
					"created_at":     schema.StringAttribute{Computed: true, MarkdownDescription: "Creation time (RFC 3339)."},
				}}},
		},
	}
}

func (d *invoicesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *invoicesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	// limit=100 keeps the API's broken `id <` cursor latent (one invoice per
	// month → reached only past 100 invoices); collectAll's next == cursor
	// guard stops a repeating cursor (T14-11).
	invoices, err := collectAll(ctx, func(ctx context.Context, cursor string) ([]invoiceWire, string, error) {
		c, err := uuidCursorParam(cursor)
		if err != nil {
			return nil, "", err
		}
		limit := invoiceListDSPageSize
		res, err := d.pd.Client.ListInvoicesWithResponse(ctx, d.pd.orgID, &client.ListInvoicesParams{Limit: &limit, Cursor: c})
		if err != nil {
			return nil, "", err
		}
		var diags diag.Diagnostics
		if res.StatusCode() != http.StatusOK {
			apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.HTTPResponse.Header, res.Body), &diags, path.Empty())
			return nil, "", pageFailure(diags)
		}
		var page struct {
			Items      *[]invoiceWire `json:"items"`
			NextCursor *string        `json:"next_cursor"`
		}
		if err := json.Unmarshal(res.Body, &page); err != nil || page.Items == nil {
			diags.AddError("Aether API returned no usable invoice list", describeUndecodable(res.HTTPResponse, res.Status(), err, "items"))
			return nil, "", pageFailure(diags)
		}
		for _, inv := range *page.Items {
			if inv.ID == "" || inv.InvoiceNumber == "" || inv.PeriodStart == nil || inv.PeriodEnd == nil || inv.CreatedAt == nil {
				diags.AddError("Aether API returned no usable invoice list",
					describeJSONWithout(res.HTTPResponse, res.Status(), "invoice id, invoice_number, period or created_at"))
				return nil, "", pageFailure(diags)
			}
		}
		next, err := nextUUIDCursor(page.NextCursor)
		if err != nil {
			return nil, "", fmt.Errorf("invoice list: %w", err)
		}
		return *page.Items, next, nil
	})
	if err != nil {
		addPageError(&resp.Diagnostics, "Cannot list invoices", err)
		return
	}
	out := invoicesModel{Invoices: make([]invoiceDataModel, 0, len(invoices))}
	for _, inv := range invoices {
		out.Invoices = append(out.Invoices, invoiceDataModel{
			ID:            types.StringValue(inv.ID),
			InvoiceNumber: types.StringValue(inv.InvoiceNumber),
			PeriodStart:   types.StringValue(inv.PeriodStart.Format(time.RFC3339)),
			PeriodEnd:     types.StringValue(inv.PeriodEnd.Format(time.RFC3339)),
			SubtotalCents: types.Int64Value(inv.SubtotalCents),
			TaxCents:      types.Int64Value(inv.TaxCents),
			TotalCents:    types.Int64Value(inv.TotalCents),
			TaxRateBP:     types.Int64Value(inv.TaxRateBP),
			TaxNote:       types.StringValue(inv.TaxNote),
			Status:        types.StringValue(inv.Status),
			CreatedAt:     types.StringValue(inv.CreatedAt.Format(time.RFC3339)),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
