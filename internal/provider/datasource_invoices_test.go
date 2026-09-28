package provider_test

// Tests for the aether_invoices data source (datasource_invoices.go, T14-11),
// registered as NewInvoicesDataSource in provider.go's DataSources().

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

const invoicesDataSource = "data.aether_invoices.i"

const invoicesDSHCL = `
provider "aether" {}
data "aether_invoices" "i" {}
`

// IN1. Seeds 150 invoices (IDs and periods aligned so the broken `id <
// cursor` predicate coincidentally still yields the correct set - this test
// is about the PROVIDER'S paging conformance, not the platform's cursor
// defect, which fakeapi's own TestInvoicesListPage2CursorIsWrong already
// documents in isolation). Checks: every raw-decoded field the generated
// client drops (invoice_number/tax_rate_bp/tax_note) is present, money is
// Int64 cents, `buyer`/`issuer`/`pdf_url` are ABSENT from state (T14-11: PII
// / never written), and the data source walked (at least) two pages with
// limit=100.
func TestInvoicesDataSource_ReadRawFieldsNoPIITwoPages(t *testing.T) {
	api := setupFake(t)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var firstID string
	for i := 0; i < 150; i++ {
		// IDs ASCEND with i (i.e. with period_start): id(newer) > id(older).
		// Filtering `id < cursor` against cursor = the last (oldest-of-page-1)
		// item's id then correctly selects exactly the still-older,
		// not-yet-returned invoices - so the real predicate (ordered by
		// period_start DESC, filtered on id) coincides with the correct
		// answer here. This is deliberate: this test is about the PROVIDER's
		// paging conformance, not the platform's cursor defect, which
		// fakeapi's own TestInvoicesListPage2CursorIsWrong documents in
		// isolation with ids constructed to DISAGREE with time order instead.
		id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", i)
		start := base.AddDate(0, i, 0)
		seeded := api.SeedInvoice(testOrg, fakeapi.InvoiceSeed{
			ID: id, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0),
			InvoiceNumber: fmt.Sprintf("AE-2020-%04d", i),
			SubtotalCents: 10000 + int64(i), TaxCents: 1900, TotalCents: 11900 + int64(i),
			TaxRateBP: 1900, TaxNote: "VAT 19%", Status: "issued",
			Issuer: map[string]any{"legal_name": "Aether SRL"},
			Buyer:  map[string]any{"billing_email": "buyer@example.com"},
		})
		if i == 0 {
			firstID = seeded
		}
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: invoicesDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(invoicesDataSource, "invoices.#", "150"),
				findInvoiceByID(invoicesDataSource, firstID, func(inv map[string]string) error {
					if inv["invoice_number"] != "AE-2020-0000" {
						return fmt.Errorf("invoice_number = %q, want AE-2020-0000", inv["invoice_number"])
					}
					if inv["tax_rate_bp"] != "1900" {
						return fmt.Errorf("tax_rate_bp = %q, want 1900", inv["tax_rate_bp"])
					}
					if inv["tax_note"] != "VAT 19%" {
						return fmt.Errorf("tax_note = %q, want %q", inv["tax_note"], "VAT 19%")
					}
					if inv["subtotal_cents"] != "10000" || inv["total_cents"] != "11900" {
						return fmt.Errorf("subtotal_cents/total_cents = %q/%q, want 10000/11900", inv["subtotal_cents"], inv["total_cents"])
					}
					if inv["status"] != "issued" {
						return fmt.Errorf("status = %q, want issued", inv["status"])
					}
					return nil
				}),
				checkAbsentFromStateSubstring(invoicesDataSource, "buyer"),
				checkAbsentFromStateSubstring(invoicesDataSource, "issuer"),
				checkAbsentFromStateSubstring(invoicesDataSource, "pdf_url"),
				func(*terraform.State) error {
					gets := requestsMatching(api, http.MethodGet, `/billing/invoices$`)
					if len(gets) < 2 {
						return fmt.Errorf("GET /billing/invoices calls = %d, want >= 2 (150 invoices, limit=100 per T14-11)", len(gets))
					}
					if !hasQueryPair(gets[0].Query, "limit", "100") {
						return fmt.Errorf("first GET query = %q, want limit=100", gets[0].Query)
					}
					return nil
				},
			),
		}},
	})
}

// findInvoiceByID scans invoices.N.* in the flat state for the entry whose id
// matches, and hands its per-field map (keys with the "invoices.N." prefix
// stripped) to check - list ORDER is not asserted here (unlike the
// repositories/tags sources, invoices has no controller ruling requiring a
// specific client-side order), only that the seeded fields survive
// correctly wherever the item landed.
func findInvoiceByID(addr, id string, check func(map[string]string) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		a := attrs(s, addr)
		n, _ := a["invoices.#"]
		if n == "" {
			return fmt.Errorf("%s has no invoices.# attribute", addr)
		}
		for i := 0; ; i++ {
			prefix := fmt.Sprintf("invoices.%d.", i)
			idKey := prefix + "id"
			got, ok := a[idKey]
			if !ok {
				break
			}
			if got == id {
				item := map[string]string{}
				for k, v := range a {
					if len(k) > len(prefix) && k[:len(prefix)] == prefix {
						item[k[len(prefix):]] = v
					}
				}
				return check(item)
			}
		}
		return fmt.Errorf("%s: no invoice with id %q found", addr, id)
	}
}
