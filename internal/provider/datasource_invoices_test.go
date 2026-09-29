package provider_test

// Tests for the aether_invoices data source (datasource_invoices.go, T14-11),
// registered as NewInvoicesDataSource in provider.go's DataSources().

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider"
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
					// M-7: the original assertion set skipped id, period_start,
					// period_end, created_at and tax_cents entirely - a mutation
					// dropping any one of those five from invoiceDataModel/the
					// flatten would have passed.
					if inv["id"] != firstID {
						return fmt.Errorf("id = %q, want %q", inv["id"], firstID)
					}
					if inv["invoice_number"] != "AE-2020-0000" {
						return fmt.Errorf("invoice_number = %q, want AE-2020-0000", inv["invoice_number"])
					}
					wantStart := base.Format(time.RFC3339)
					if inv["period_start"] != wantStart {
						return fmt.Errorf("period_start = %q, want %q", inv["period_start"], wantStart)
					}
					wantEnd := base.AddDate(0, 1, 0).Format(time.RFC3339)
					if inv["period_end"] != wantEnd {
						return fmt.Errorf("period_end = %q, want %q", inv["period_end"], wantEnd)
					}
					if _, err := time.Parse(time.RFC3339, inv["created_at"]); err != nil {
						return fmt.Errorf("created_at = %q is not a valid RFC 3339 timestamp: %s", inv["created_at"], err)
					}
					if inv["tax_cents"] != "1900" {
						return fmt.Errorf("tax_cents = %q, want 1900", inv["tax_cents"])
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

// M-6: a non-200 (502, an upstream gateway failure) from the invoices route
// must surface as a plan-time error carrying the status and message.
func TestInvoicesDataSource_APIErrorSurfaces(t *testing.T) {
	api := setupFake(t)
	route := "GET /organizations/" + testOrg + "/billing/invoices"
	api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
		fakeapi.WriteJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"code": "upstream_error", "message": "billing service unavailable"}})
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      invoicesDSHCL,
			ExpectError: loose("502 upstream_error: billing service unavailable"),
		}},
	})
}

// Coder's suggested pin 3 (task-14-report.md "Fix round 2", review r1 M-2):
// the schema description must cite the tracking issue, not just describe the
// defect in prose that could silently drift. Calls NewInvoicesDataSource's
// Schema method directly (reachable from this external test package since
// it, like the schema.Schema type, is exported), so this fails on the exact
// text going missing or being reworded without a matching test update.
func TestInvoicesDataSource_SchemaDescriptionCitesAPIIssue(t *testing.T) {
	var resp datasource.SchemaResponse
	provider.NewInvoicesDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	const want = "aether-api #318"
	if !strings.Contains(resp.Schema.MarkdownDescription, want) {
		t.Fatalf("aether_invoices description = %q, want it to contain %q", resp.Schema.MarkdownDescription, want)
	}
}

// T14-15 (the same collectAll pager family as
// TestOrganizationMembersDataSource_RepeatedCursorErrors, distinct from the
// Harbor-page pager): a server that repeats next_cursor must be an error
// here too, not a silently truncated/looping list (ruling T14-15, review r1
// M-1). Named as a coverage gap in task-14-tests-report.md's "Fix round 1".
func TestInvoicesDataSource_RepeatedCursorErrors(t *testing.T) {
	api := setupFake(t)
	id := api.SeedInvoice(testOrg, fakeapi.InvoiceSeed{
		PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		SubtotalCents: 100, TotalCents: 100, Status: "issued",
	})
	api.SetRepeatingInvoiceCursor(testOrg, id)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      invoicesDSHCL,
			ExpectError: loose("repeated"),
		}},
	})
}
