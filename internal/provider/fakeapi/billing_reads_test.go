package fakeapi

// The fake's own tests for billing_reads.go (SDD
// task-14-measured-contract.md §4-§6). testOrg/newClusterFake/call/decode are
// shared helpers from clusters_test.go (same package).
//
// Defect classes each test below is written to catch:
//   D1 an empty/never-Set org answers an error instead of a genuine zero
//   D2 the usage key is "autoscaling" (or anything but "items") - or items
//      is rendered as the wrong shape
//   D3 a breakdown storage line's node_pool_id renders as null/"" instead
//      of being OMITTED, or pool_name/resource_type are swapped
//   D4 pdf_url leaks onto the wire, or buyer/issuer render even when the
//      seed never supplied them
//   D5 the invoice list's cursor defect is "fixed" (same class as the
//      audit log's documentation test)
//   D6 money typing wrong (float64 vs an accidental string/int)

import (
	"net/http"
	"testing"
	"time"
)

const (
	usagePath     = "/organizations/" + testOrg + "/billing/usage"
	breakdownPath = "/organizations/" + testOrg + "/billing/breakdown"
	invoicesPath  = "/organizations/" + testOrg + "/billing/invoices"
)

// D1: an org nothing was ever Set for reads back a genuine empty month -
// 200, not 404/500, every list empty (never null on the wire), total 0.
func TestUsageNeverSetIsEmptyMonthNotError(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodGet, usagePath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET usage (never set) = %d %s, want 200", resp.StatusCode, b)
	}
	m := decode(t, b)
	items, _ := m["items"].([]any)
	subs, _ := m["subscriptions"].([]any)
	if len(items) != 0 || len(subs) != 0 {
		t.Fatalf("items/subscriptions = %v/%v, want both empty", m["items"], m["subscriptions"])
	}
	if total, ok := m["total_eur"].(float64); !ok || total != 0 {
		t.Fatalf("total_eur = %v, want float64 0", m["total_eur"])
	}
}

// D2/D6: SetUsage's items/subscriptions/total_eur render under the RIGHT
// keys ("items", never "autoscaling") with float64 money.
func TestUsageSetRendersItemsKeyAndSubscriptions(t *testing.T) {
	s := newClusterFake(t)
	clusterID := "44444444-4444-4444-8444-444444444444"
	s.SetUsage(testOrg,
		[]map[string]any{{"id": "u1", "resource_type": "registry", "quantity": 1.5, "unit": "gib-hour"}},
		[]SubscriptionLine{{Kind: "plan", ClusterID: &clusterID, Name: "starter", AmountEUR: 10, GrossAmountEUR: 10, DiscountEUR: 0}},
		10.5,
	)
	resp, b := call(t, s, http.MethodGet, usagePath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET usage = %d %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	if _, has := m["autoscaling"]; has {
		t.Fatalf("body carries an \"autoscaling\" key = %v, want NO such key (the real key is \"items\")", m["autoscaling"])
	}
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want 1", items)
	}
	subs, _ := m["subscriptions"].([]any)
	if len(subs) != 1 {
		t.Fatalf("subscriptions = %v, want 1", subs)
	}
	sub := subs[0].(map[string]any)
	if sub["cluster_id"] != clusterID || sub["kind"] != "plan" || sub["name"] != "starter" {
		t.Fatalf("subscription = %v, want cluster_id/kind/name set", sub)
	}
	if _, has := sub["node_pool_id"]; has {
		t.Fatalf("subscription carries node_pool_id = %v, want OMITTED (nil pointer)", sub["node_pool_id"])
	}
	total, ok := m["total_eur"].(float64)
	if !ok || total != 10.5 {
		t.Fatalf("total_eur = %v (type %T), want float64 10.5", m["total_eur"], m["total_eur"])
	}
}

// D1: breakdown mirrors usage's empty-month contract independently.
func TestBreakdownNeverSetIsEmptyMonthNotError(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodGet, breakdownPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET breakdown (never set) = %d %s, want 200", resp.StatusCode, b)
	}
	m := decode(t, b)
	subs, _ := m["subscriptions"].([]any)
	auto, _ := m["autoscaling"].([]any)
	if len(subs) != 0 || len(auto) != 0 {
		t.Fatalf("subscriptions/autoscaling = %v/%v, want both empty", m["subscriptions"], m["autoscaling"])
	}
	if sub, ok := m["subtotal_eur"].(float64); !ok || sub != 0 {
		t.Fatalf("subtotal_eur = %v, want float64 0", m["subtotal_eur"])
	}
}

// D3: a storage line has node_pool_id OMITTED (not null - openapi_types.UUID
// has no zero wire form) and pool_name holding a CLUSTER name, with a
// resource_type value OUTSIDE the vcpu/memory/disk enum.
func TestBreakdownStorageLineShapeAndOutOfEnumResourceType(t *testing.T) {
	s := newClusterFake(t)
	poolID := "55555555-5555-4555-8555-555555555555"
	s.SetBreakdown(testOrg, nil, []AutoscalingLine{
		{NodePoolID: &poolID, PoolName: "workers", ResourceType: "vcpu", Unit: "vcpu-hour", Quantity: 10, UnitPriceEUR: 0.01, AmountEUR: 0.1},
		{NodePoolID: nil, PoolName: "prod-cluster", ResourceType: "registry", Unit: "gib-hour", Quantity: 5, UnitPriceEUR: 0.02, AmountEUR: 0.1},
	}, 0.2)
	_, b := call(t, s, http.MethodGet, breakdownPath, "")
	m := decode(t, b)
	auto, _ := m["autoscaling"].([]any)
	if len(auto) != 2 {
		t.Fatalf("autoscaling = %v, want 2 lines", auto)
	}
	var poolLine, storageLine map[string]any
	for _, l := range auto {
		line := l.(map[string]any)
		if line["resource_type"] == "vcpu" {
			poolLine = line
		} else {
			storageLine = line
		}
	}
	if poolLine == nil || storageLine == nil {
		t.Fatalf("autoscaling = %v, want one pool line and one storage line", auto)
	}
	if poolLine["node_pool_id"] != poolID {
		t.Fatalf("pool line node_pool_id = %v, want %q", poolLine["node_pool_id"], poolID)
	}
	if _, has := storageLine["node_pool_id"]; has {
		t.Fatalf("storage line carries node_pool_id = %v, want OMITTED", storageLine["node_pool_id"])
	}
	if storageLine["pool_name"] != "prod-cluster" {
		t.Fatalf("storage line pool_name = %v, want the CLUSTER name %q", storageLine["pool_name"], "prod-cluster")
	}
	if storageLine["resource_type"] != "registry" {
		t.Fatalf("storage line resource_type = %v, want %q (outside the vcpu/memory/disk enum, on purpose)", storageLine["resource_type"], "registry")
	}
}

// D4: pdf_url is NEVER on the wire; buyer/issuer render exactly when seeded,
// and are entirely absent otherwise (proving a data source COULD leave them
// out only if it chooses to, not because the fake never sent them).
func TestInvoicesListPdfURLNeverPresentBuyerIssuerOnlyWhenSeeded(t *testing.T) {
	s := newClusterFake(t)
	s.SeedInvoice(testOrg, InvoiceSeed{
		PeriodStart: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		SubtotalCents: 1000, TaxCents: 190, TotalCents: 1190, TaxRateBP: 1900, TaxNote: "VAT 19%", Status: "issued",
	})
	s.SeedInvoice(testOrg, InvoiceSeed{
		PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		SubtotalCents: 2000, TaxCents: 380, TotalCents: 2380, TaxRateBP: 1900, TaxNote: "VAT 19%", Status: "paid",
		Issuer: map[string]any{"legal_name": "Aether SRL"},
		Buyer:  map[string]any{"billing_email": "buyer@example.com"},
	})
	_, b := call(t, s, http.MethodGet, invoicesPath, "")
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want 2", items)
	}
	for _, it := range items {
		inv := it.(map[string]any)
		if _, has := inv["pdf_url"]; has {
			t.Fatalf("invoice carries pdf_url = %v, want it NEVER present", inv["pdf_url"])
		}
	}
	var bare, full map[string]any
	for _, it := range items {
		inv := it.(map[string]any)
		if inv["status"] == "issued" {
			bare = inv
		} else {
			full = inv
		}
	}
	if _, has := bare["issuer"]; has {
		t.Fatalf("unseeded invoice carries issuer = %v, want OMITTED", bare["issuer"])
	}
	if _, has := bare["buyer"]; has {
		t.Fatalf("unseeded invoice carries buyer = %v, want OMITTED", bare["buyer"])
	}
	issuer, ok := full["issuer"].(map[string]any)
	if !ok || issuer["legal_name"] != "Aether SRL" {
		t.Fatalf("seeded invoice issuer = %v, want the seeded object", full["issuer"])
	}
	buyer, ok := full["buyer"].(map[string]any)
	if !ok || buyer["billing_email"] != "buyer@example.com" {
		t.Fatalf("seeded invoice buyer = %v, want the seeded object", full["buyer"])
	}
}

// D6: cents are integral (int64 on the wire, decoded here as float64 with no
// fractional part) - not a string.
func TestInvoicesListMoneyIsIntegralCents(t *testing.T) {
	s := newClusterFake(t)
	s.SeedInvoice(testOrg, InvoiceSeed{
		PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), PeriodEnd: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		SubtotalCents: 123456, TaxCents: 0, TotalCents: 123456, Status: "issued",
	})
	_, b := call(t, s, http.MethodGet, invoicesPath, "")
	m := decode(t, b)
	items, _ := m["items"].([]any)
	inv := items[0].(map[string]any)
	total, ok := inv["total_cents"].(float64)
	if !ok || total != 123456 {
		t.Fatalf("total_cents = %v (type %T), want the integer 123456", inv["total_cents"], inv["total_cents"])
	}
}

// D5 (documentation test, same class as the audit log's): the invoice list
// shares the SAME broken `id < cursor` predicate, ordered by period_start
// DESC while filtering on an unrelated id. If this test starts passing with
// a CORRECT page 2, the shared brokenCursorPage helper (or this route's use
// of it) has been quietly repaired - which must not happen silently, since
// the fake exists to model the real, currently-unfixed platform defect
// (contract §6, aether-api issue A).
func TestInvoicesListPage2CursorIsWrong(t *testing.T) {
	s := newClusterFake(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mk := func(id string, monthOffset int) string {
		start := base.AddDate(0, monthOffset, 0)
		return s.SeedInvoice(testOrg, InvoiceSeed{
			ID: id, PeriodStart: start, PeriodEnd: start.AddDate(0, 1, 0),
			SubtotalCents: 100, TotalCents: 100, Status: "issued",
		})
	}
	i1 := mk("90000000-0000-4000-8000-000000000000", 0) // Jan, oldest
	i2 := mk("10000000-0000-4000-8000-000000000000", 1) // Feb
	i3 := mk("80000000-0000-4000-8000-000000000000", 2) // Mar
	i4 := mk("20000000-0000-4000-8000-000000000000", 3) // Apr
	i5 := mk("70000000-0000-4000-8000-000000000000", 4) // May, newest

	_, b1 := call(t, s, http.MethodGet, invoicesPath+"?limit=2", "")
	m1 := decode(t, b1)
	items1, _ := m1["items"].([]any)
	if len(items1) != 2 || items1[0].(map[string]any)["id"] != i5 || items1[1].(map[string]any)["id"] != i4 {
		t.Fatalf("page1 = %v, want the newest two [%s,%s]", items1, i5, i4)
	}
	cursor, _ := m1["next_cursor"].(string)
	if cursor != i4 {
		t.Fatalf("page1 next_cursor = %q, want i4's id %q", cursor, i4)
	}
	_, b2 := call(t, s, http.MethodGet, invoicesPath+"?limit=2&cursor="+cursor, "")
	m2 := decode(t, b2)
	items2, _ := m2["items"].([]any)
	if len(items2) != 1 || items2[0].(map[string]any)["id"] != i2 {
		t.Fatalf("page2 = %v, want the WRONG (but faithfully reproduced) [%s] - a correct page2 would be [%s,%s]", items2, i2, i3, i2)
	}
	_ = i1
	_ = i3
}
