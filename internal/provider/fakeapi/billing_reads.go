package fakeapi

// Billing READ routes of the fake (usage, cost breakdown, invoices),
// reproducing what aether-api DOES (measured at aether-api 8e9045a; SDD
// task-14-measured-contract.md §4-§6), not what its OpenAPI spec says. The
// budget route (a different table) stays in budgets.go.
//
//	GET /organizations/{org}/billing/usage                    200 UsageSummary
//	GET /organizations/{org}/billing/breakdown                200 CostBreakdown
//	GET /organizations/{org}/billing/invoices[?cursor=&limit=] 200 {items[],next_cursor?}
//
// Behaviours that matter and that the fake copies:
//   - usage and breakdown take NO parameters and always answer for the SAME
//     fixed "current month" window (billingEpoch..billingEpochEnd) - there is
//     no month input anywhere on the real routes either (§4/§5);
//   - an org nothing has ever been Set for reads back a genuine EMPTY month:
//     200, empty items/subscriptions/autoscaling, every total 0 - never an
//     error (§4/§5, "a genuine zero, not a could-not-look");
//   - usage.items carries EVERY usage_records row of the month, storage
//     included, unaggregated (§4) - modelled as an opaque
//     []map[string]any the seed provides verbatim, since no data source in
//     this task reads it (T14-9 drops it) and the fake only needs to prove
//     the KEY is "items", not "autoscaling";
//   - breakdown.autoscaling mixes per-pool lines with storage lines: storage
//     lines have node_pool_id NULL (omitted - openapi_types.UUID has no zero
//     JSON form) and pool_name holding a CLUSTER name or "(deleted
//     cluster)", with resource_type values (registry/storage_block/
//     storage_rwx) OUTSIDE the OA/CG enum (§5) - AutoscalingLine below models
//     exactly that (NodePoolID *string, omitted when nil);
//   - money is ALWAYS float64 euros here (never a string) and invoice money
//     is ALWAYS int64 cents (§4-§6, T14-13);
//   - the invoice list shares the SAME broken cursor defect as the audit log
//     (contract §6, aether-api issue A): order `period_start DESC, id DESC`,
//     predicate `id < cursor` - reuses brokenCursorPage from audit_log.go
//     verbatim;
//   - invoice `pdf_url` is NEVER written (§6) - InvoiceSeed has no field for
//     it, so it is structurally impossible for a test to seed one by
//     accident; `buyer`/`issuer` ARE rendered on the wire (real PII the fake
//     must reproduce so a test can assert a correct data source leaves them
//     OUT of state) but only when the seed supplies them - both stay nil by
//     default so a test not exercising them isn't forced to fabricate PII;
//   - `status` is stored verbatim, no default and no validation (the fake
//     does not attempt to reproduce which of draft/issued/paid/void has a
//     real writer, §6) - a test picks the value it needs.

import (
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	usageRoutePath     = regexp.MustCompile(`^/organizations/([^/]+)/billing/usage$`)
	breakdownRoutePath = regexp.MustCompile(`^/organizations/([^/]+)/billing/breakdown$`)
	invoicesRoutePath  = regexp.MustCompile(`^/organizations/([^/]+)/billing/invoices$`)
)

// billingEpoch/billingEpochEnd stand in for "the current UTC month" the real
// routes always answer for (§4/§5) - only their VALUE as period_start/
// period_end and their relative ordering matter to a caller, never these
// particular dates.
var (
	billingEpoch    = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	billingEpochEnd = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

// SubscriptionLine is a flat monthly line, shared by usage and breakdown
// (contract §4/§5).
type SubscriptionLine struct {
	Kind                                   string
	ClusterID, NodePoolID                  *string
	Name                                   string
	AmountEUR, GrossAmountEUR, DiscountEUR float64
}

// AutoscalingLine is one line of breakdown.autoscaling: either a per-pool
// line (NodePoolID set) or a storage line (NodePoolID nil, PoolName holding a
// CLUSTER name, ResourceType one of registry/storage_block/storage_rwx -
// contract §5).
type AutoscalingLine struct {
	NodePoolID                        *string
	PoolName, ResourceType, Unit      string
	Quantity, UnitPriceEUR, AmountEUR float64
}

type fakeUsage struct {
	items         []map[string]any
	subscriptions []SubscriptionLine
	totalEUR      float64
}

type fakeBreakdown struct {
	subscriptions []SubscriptionLine
	autoscaling   []AutoscalingLine
	subtotalEUR   float64
}

// InvoiceSeed is one invoice to seed via SeedInvoice. ID/InvoiceNumber are
// generated when empty; CreatedAt defaults to an auto-incrementing clock.
// There is deliberately NO PdfUrl field (§6 - never written by any code
// path).
type InvoiceSeed struct {
	ID, InvoiceNumber                   string
	PeriodStart, PeriodEnd              time.Time
	SubtotalCents, TaxCents, TotalCents int64
	TaxRateBP                           int
	TaxNote, Status                     string
	Issuer, Buyer                       map[string]any // nil = omit the key entirely
	CreatedAt                           time.Time
}

type fakeInvoice struct {
	id, invoiceNumber, orgID            string
	periodStart, periodEnd              time.Time
	subtotalCents, taxCents, totalCents int64
	taxRateBP                           int
	taxNote, status                     string
	issuer, buyer                       map[string]any
	createdAt                           time.Time
}

// billingReadStore is embedded by value in Server; its zero value is usable
// (a never-Set org reads back the empty-month/empty-invoice-list shape).
type billingReadStore struct {
	mu        sync.Mutex
	usage     map[string]*fakeUsage
	breakdown map[string]*fakeBreakdown
	invoices  map[string][]*fakeInvoice
	seq       int

	// forceCursor, keyed by org, when present makes the invoice list report
	// this value as next_cursor on EVERY page, regardless of the natural
	// (broken) cursor arithmetic brokenCursorPage computes - a fake server
	// contract violation used to prove the data source's collectAll pager
	// fails loud on a repeated cursor instead of looping/truncating (ruling
	// T14-15, review r1 M-1). See SetRepeatingInvoiceCursor.
	forceCursor map[string]string
}

// SetUsage replaces org's billing/usage answer wholesale. items is rendered
// verbatim (each element must already be a JSON-safe map - the fake does not
// interpret it); a nil/empty items or subscriptions still renders as `[]`,
// never omitted or null (§4).
func (s *Server) SetUsage(org string, items []map[string]any, subscriptions []SubscriptionLine, totalEUR float64) {
	br := &s.billing
	br.mu.Lock()
	defer br.mu.Unlock()
	if br.usage == nil {
		br.usage = map[string]*fakeUsage{}
	}
	br.usage[org] = &fakeUsage{items: items, subscriptions: subscriptions, totalEUR: totalEUR}
}

// SetBreakdown replaces org's billing/breakdown answer wholesale.
func (s *Server) SetBreakdown(org string, subscriptions []SubscriptionLine, autoscaling []AutoscalingLine, subtotalEUR float64) {
	br := &s.billing
	br.mu.Lock()
	defer br.mu.Unlock()
	if br.breakdown == nil {
		br.breakdown = map[string]*fakeBreakdown{}
	}
	br.breakdown[org] = &fakeBreakdown{subscriptions: subscriptions, autoscaling: autoscaling, subtotalEUR: subtotalEUR}
}

// SeedInvoice adds an invoice out of band (no request recorded, no
// validation - it can hold a PeriodStart that collides with another seeded
// invoice, which a real (org,period_start) UNIQUE constraint would refuse)
// and returns its id.
func (s *Server) SeedInvoice(org string, inv InvoiceSeed) string {
	br := &s.billing
	br.mu.Lock()
	defer br.mu.Unlock()
	if br.invoices == nil {
		br.invoices = map[string][]*fakeInvoice{}
	}
	br.seq++
	id := inv.ID
	if id == "" {
		id = uuid.NewString()
	}
	number := inv.InvoiceNumber
	if number == "" {
		number = "AE-2026-" + padInvoiceSeq(br.seq)
	}
	created := inv.CreatedAt
	if created.IsZero() {
		created = billingEpoch.Add(time.Duration(br.seq) * time.Second)
	}
	fi := &fakeInvoice{
		id: id, invoiceNumber: number, orgID: org,
		periodStart: inv.PeriodStart, periodEnd: inv.PeriodEnd,
		subtotalCents: inv.SubtotalCents, taxCents: inv.TaxCents, totalCents: inv.TotalCents,
		taxRateBP: inv.TaxRateBP, taxNote: inv.TaxNote, status: inv.Status,
		issuer: inv.Issuer, buyer: inv.Buyer, createdAt: created,
	}
	br.invoices[org] = append(br.invoices[org], fi)
	return id
}

// SetRepeatingInvoiceCursor forces the aether_invoices list to report cursor
// as next_cursor on EVERY page for org, regardless of what brokenCursorPage
// would naturally compute. cursor must be a UUID the caller controls (e.g. a
// SeedInvoice id), since nextUUIDCursor parses next_cursor as one.
func (s *Server) SetRepeatingInvoiceCursor(org, cursor string) {
	br := &s.billing
	br.mu.Lock()
	defer br.mu.Unlock()
	if br.forceCursor == nil {
		br.forceCursor = map[string]string{}
	}
	br.forceCursor[org] = cursor
}

func padInvoiceSeq(n int) string {
	digits := "0000" + strconv.Itoa(n)
	return digits[len(digits)-4:]
}

func subscriptionLineJSON(l SubscriptionLine) map[string]any {
	m := map[string]any{
		"kind": l.Kind, "name": l.Name,
		"amount_eur": l.AmountEUR, "gross_amount_eur": l.GrossAmountEUR, "discount_eur": l.DiscountEUR,
	}
	if l.ClusterID != nil {
		m["cluster_id"] = *l.ClusterID
	}
	if l.NodePoolID != nil {
		m["node_pool_id"] = *l.NodePoolID
	}
	return m
}

func autoscalingLineJSON(l AutoscalingLine) map[string]any {
	m := map[string]any{
		"pool_name": l.PoolName, "resource_type": l.ResourceType, "unit": l.Unit,
		"quantity": l.Quantity, "unit_price_eur": l.UnitPriceEUR, "amount_eur": l.AmountEUR,
	}
	if l.NodePoolID != nil {
		m["node_pool_id"] = *l.NodePoolID
	}
	return m
}

func usageJSON(org string, u *fakeUsage) map[string]any {
	items := u.items
	if items == nil {
		items = []map[string]any{}
	}
	subs := make([]map[string]any, 0, len(u.subscriptions))
	for _, l := range u.subscriptions {
		subs = append(subs, subscriptionLineJSON(l))
	}
	return map[string]any{
		"org_id": org, "period_start": billingEpoch.Format(time.RFC3339), "period_end": billingEpochEnd.Format(time.RFC3339),
		"items": items, "subscriptions": subs, "total_eur": u.totalEUR,
	}
}

func breakdownJSON(org string, b *fakeBreakdown) map[string]any {
	subs := make([]map[string]any, 0, len(b.subscriptions))
	for _, l := range b.subscriptions {
		subs = append(subs, subscriptionLineJSON(l))
	}
	auto := make([]map[string]any, 0, len(b.autoscaling))
	for _, l := range b.autoscaling {
		auto = append(auto, autoscalingLineJSON(l))
	}
	return map[string]any{
		"org_id": org, "period_start": billingEpoch.Format(time.RFC3339), "period_end": billingEpochEnd.Format(time.RFC3339),
		"subscriptions": subs, "autoscaling": auto, "subtotal_eur": b.subtotalEUR,
	}
}

func invoiceJSON(inv *fakeInvoice) map[string]any {
	m := map[string]any{
		"id": inv.id, "invoice_number": inv.invoiceNumber, "org_id": inv.orgID,
		"period_start": inv.periodStart.Format(time.RFC3339), "period_end": inv.periodEnd.Format(time.RFC3339),
		"subtotal_cents": inv.subtotalCents, "tax_cents": inv.taxCents, "total_cents": inv.totalCents,
		"tax_rate_bp": inv.taxRateBP, "tax_note": inv.taxNote, "status": inv.status,
		"created_at": inv.createdAt.Format(time.RFC3339),
		// pdf_url: NEVER included (§6).
	}
	if inv.issuer != nil {
		m["issuer"] = inv.issuer
	}
	if inv.buyer != nil {
		m["buyer"] = inv.buyer
	}
	return m
}

// routeBillingReads serves usage/breakdown/invoices; it reports false when
// path is not one of them.
func (s *Server) routeBillingReads(w http.ResponseWriter, r *http.Request, path string) bool {
	if m := usageRoutePath.FindStringSubmatch(path); m != nil {
		org := m[1]
		if !s.orgAccess(w, org) {
			return true
		}
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
			return true
		}
		br := &s.billing
		br.mu.Lock()
		u, ok := br.usage[org]
		br.mu.Unlock()
		if !ok {
			u = &fakeUsage{} // empty month: zeros, never an error (§4)
		}
		writeJSON(w, http.StatusOK, usageJSON(org, u))
		return true
	}
	if m := breakdownRoutePath.FindStringSubmatch(path); m != nil {
		org := m[1]
		if !s.orgAccess(w, org) {
			return true
		}
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
			return true
		}
		br := &s.billing
		br.mu.Lock()
		b, ok := br.breakdown[org]
		br.mu.Unlock()
		if !ok {
			b = &fakeBreakdown{} // empty month: zeros, never an error (§5)
		}
		writeJSON(w, http.StatusOK, breakdownJSON(org, b))
		return true
	}
	if m := invoicesRoutePath.FindStringSubmatch(path); m != nil {
		org := m[1]
		if !s.orgAccess(w, org) {
			return true
		}
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
			return true
		}
		s.listInvoices(w, r, org)
		return true
	}
	return false
}

// listInvoices is GET .../billing/invoices (§6): order period_start DESC, id
// DESC, the SAME broken `id < cursor` predicate as the audit log
// (brokenCursorPage, audit_log.go).
func (s *Server) listInvoices(w http.ResponseWriter, r *http.Request, org string) {
	br := &s.billing
	br.mu.Lock()
	all := append([]*fakeInvoice(nil), br.invoices[org]...)
	forced, forcedOK := br.forceCursor[org]
	br.mu.Unlock()
	limit := auditLimit(r.URL.Query().Get("limit")) // identical 1..100/default-50 rule (§6)
	cursor := r.URL.Query().Get("cursor")
	less := func(a, b *fakeInvoice) bool {
		if !a.periodStart.Equal(b.periodStart) {
			return a.periodStart.After(b.periodStart) // period_start DESC
		}
		return a.id > b.id // id DESC tiebreak
	}
	page, next := brokenCursorPage(all, func(i *fakeInvoice) string { return i.id }, less, cursor, limit)
	if forcedOK {
		next = forced
	}
	items := make([]map[string]any, 0, len(page))
	for _, inv := range page {
		items = append(items, invoiceJSON(inv))
	}
	out := map[string]any{"items": items}
	if next != "" {
		out["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, out)
}
