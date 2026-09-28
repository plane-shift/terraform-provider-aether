package fakeapi

// The fake's own tests for audit_log.go (SDD task-14-measured-contract.md
// §3). testOrg/newClusterFake/call/decode are shared helpers from
// clusters_test.go (same package).
//
// Defect classes each test below is written to catch:
//   D1 wrong order (not newest-first, or the id-DESC tiebreak missing)
//   D2 limit not honoured, or an invalid limit not silently becoming 50
//   D3 next_cursor present/absent at the wrong page boundary
//   D4 optional-field omission inverted (metadata/actor_id/actor_email/
//      resource_id/ip_address rendered as null instead of omitted, or
//      vice versa)
//   D5 the broken cursor "fixed" (page 2 quietly made correct instead of
//      reproducing the real defect) - the documentation test below FAILS
//      if a future edit accidentally repairs the pagination, which is
//      exactly the point: nobody should "fix" the fake into behaving
//      better than the platform it stands in for

import (
	"net/http"
	"testing"
	"time"
)

const auditLogPath = "/organizations/" + testOrg + "/audit-log"

// D1: newest first (created_at DESC), with id DESC breaking an exact tie.
func TestAuditLogListOrdersNewestFirst(t *testing.T) {
	s := newClusterFake(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	older := s.SeedAuditEvent(testOrg, AuditEventSeed{ActorType: "system", Action: "a", ResourceType: "r", CreatedAt: base})
	newer := s.SeedAuditEvent(testOrg, AuditEventSeed{ActorType: "system", Action: "b", ResourceType: "r", CreatedAt: base.Add(time.Hour)})
	_, b := call(t, s, http.MethodGet, auditLogPath, "")
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want 2", items)
	}
	if items[0].(map[string]any)["id"] != newer || items[1].(map[string]any)["id"] != older {
		t.Fatalf("order = [%v,%v], want [newer=%s, older=%s]", items[0], items[1], newer, older)
	}
}

// D2: an invalid limit ("abc", 0, 101) silently becomes 50 - not clamped to
// the nearest bound, not an error.
func TestAuditLogListInvalidLimitBecomesFifty(t *testing.T) {
	s := newClusterFake(t)
	for i := 0; i < 60; i++ {
		s.SeedAuditEvent(testOrg, AuditEventSeed{ActorType: "system", Action: "a", ResourceType: "r"})
	}
	for _, limit := range []string{"abc", "0", "101", "-5"} {
		_, b := call(t, s, http.MethodGet, auditLogPath+"?limit="+limit, "")
		m := decode(t, b)
		items, _ := m["items"].([]any)
		if len(items) != 50 {
			t.Fatalf("limit=%q items = %d, want 50 (invalid limit silently becomes the default)", limit, len(items))
		}
	}
}

// D2: a valid limit is honoured exactly.
func TestAuditLogListHonoursValidLimit(t *testing.T) {
	s := newClusterFake(t)
	for i := 0; i < 10; i++ {
		s.SeedAuditEvent(testOrg, AuditEventSeed{ActorType: "system", Action: "a", ResourceType: "r"})
	}
	_, b := call(t, s, http.MethodGet, auditLogPath+"?limit=7", "")
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 7 {
		t.Fatalf("limit=7 items = %d, want 7", len(items))
	}
}

// D3: next_cursor is present ONLY when the page came back exactly `limit`
// items long - absent on a short page, absent on an empty page.
func TestAuditLogListNextCursorOnlyOnFullPage(t *testing.T) {
	s := newClusterFake(t)
	for i := 0; i < 3; i++ {
		s.SeedAuditEvent(testOrg, AuditEventSeed{ActorType: "system", Action: "a", ResourceType: "r"})
	}
	_, bShort := call(t, s, http.MethodGet, auditLogPath+"?limit=5", "")
	mShort := decode(t, bShort)
	if _, has := mShort["next_cursor"]; has {
		t.Fatalf("short page (3 of limit=5) carries next_cursor = %v, want ABSENT", mShort["next_cursor"])
	}
	_, bFull := call(t, s, http.MethodGet, auditLogPath+"?limit=3", "")
	mFull := decode(t, bFull)
	if _, has := mFull["next_cursor"]; !has {
		t.Fatalf("full page (3 of limit=3) has no next_cursor, want it PRESENT")
	}
	empty := newClusterFake(t)
	_, bEmpty := call(t, empty, http.MethodGet, auditLogPath, "")
	mEmpty := decode(t, bEmpty)
	if _, has := mEmpty["next_cursor"]; has {
		t.Fatalf("empty org carries next_cursor = %v, want ABSENT", mEmpty["next_cursor"])
	}
}

// D4: metadata is omitted (not null) when the seed's Metadata is nil, and
// rendered verbatim (arbitrary JSON, here an object) when set. actor_id/
// actor_email/resource_id/ip_address each follow the same omit-when-nil rule
// independently of one another.
func TestAuditLogListOptionalFieldsOmittedWhenNil(t *testing.T) {
	s := newClusterFake(t)
	actorID, actorEmail, resourceID, ip := "22222222-2222-4222-8222-222222222222", "alice@example.com", "33333333-3333-4333-8333-333333333333", "203.0.113.9"
	s.SeedAuditEvent(testOrg, AuditEventSeed{ActorType: "system", Action: "bare", ResourceType: "cluster"})
	s.SeedAuditEvent(testOrg, AuditEventSeed{
		ActorType: "user", Action: "full", ResourceType: "cluster",
		ActorID: &actorID, ActorEmail: &actorEmail, ResourceID: &resourceID, IPAddress: &ip,
		Metadata: map[string]any{"k": "v"},
	})
	_, b := call(t, s, http.MethodGet, auditLogPath, "")
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want 2", items)
	}
	var bare, full map[string]any
	for _, it := range items {
		e := it.(map[string]any)
		if e["action"] == "bare" {
			bare = e
		} else {
			full = e
		}
	}
	for _, k := range []string{"actor_id", "actor_email", "resource_id", "ip_address", "metadata"} {
		if _, has := bare[k]; has {
			t.Fatalf("bare event carries %q = %v, want OMITTED", k, bare[k])
		}
	}
	if full["actor_id"] != actorID || full["actor_email"] != actorEmail || full["resource_id"] != resourceID || full["ip_address"] != ip {
		t.Fatalf("full event = %v, want actor_id/actor_email/resource_id/ip_address set verbatim", full)
	}
	md, ok := full["metadata"].(map[string]any)
	if !ok || md["k"] != "v" {
		t.Fatalf("full event metadata = %v, want {\"k\":\"v\"}", full["metadata"])
	}
}

// D5 (the brief's required documentation test): the cursor defect is real.
// Five events are seeded with ids that DISAGREE with their time order (the
// exact shape random UUIDs produce on the live platform). True newest-first
// order is e5,e4,e3,e2,e1. Page 1 (limit=2, cursor="") is CORRECT: [e5,e4].
// Page 2, following next_cursor=e4's id per the real API's own contract,
// is WRONG: it applies `id < cursor` (e4's id) with no reference to time at
// all, so it returns whichever OTHER events happen to have a lexicographically
// smaller id - here just [e2] - silently DROPPING e3 (a real event, newer
// than what page 2 did return) and never reaching e1 either. A caller that
// walks pages to "collect everything" ends up with e5,e4,e2: two rows
// silently missing. If a future change to brokenCursorPage or its callers
// quietly makes this correct, THIS test fails - intentionally: the fake
// exists to reproduce aether-api's real, currently-unfixed defect
// (contract §3, aether-api issue A), not to be a better implementation than
// the platform it stands in for.
func TestAuditLogListPage2CursorIsWrong(t *testing.T) {
	s := newClusterFake(t)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	e1 := s.SeedAuditEvent(testOrg, AuditEventSeed{ID: "90000000-0000-4000-8000-000000000000", ActorType: "system", Action: "e1", ResourceType: "r", CreatedAt: base})
	e2 := s.SeedAuditEvent(testOrg, AuditEventSeed{ID: "10000000-0000-4000-8000-000000000000", ActorType: "system", Action: "e2", ResourceType: "r", CreatedAt: base.Add(1 * time.Hour)})
	e3 := s.SeedAuditEvent(testOrg, AuditEventSeed{ID: "80000000-0000-4000-8000-000000000000", ActorType: "system", Action: "e3", ResourceType: "r", CreatedAt: base.Add(2 * time.Hour)})
	e4 := s.SeedAuditEvent(testOrg, AuditEventSeed{ID: "20000000-0000-4000-8000-000000000000", ActorType: "system", Action: "e4", ResourceType: "r", CreatedAt: base.Add(3 * time.Hour)})
	e5 := s.SeedAuditEvent(testOrg, AuditEventSeed{ID: "70000000-0000-4000-8000-000000000000", ActorType: "system", Action: "e5", ResourceType: "r", CreatedAt: base.Add(4 * time.Hour)})

	_, b1 := call(t, s, http.MethodGet, auditLogPath+"?limit=2", "")
	m1 := decode(t, b1)
	items1, _ := m1["items"].([]any)
	if len(items1) != 2 || items1[0].(map[string]any)["id"] != e5 || items1[1].(map[string]any)["id"] != e4 {
		t.Fatalf("page1 = %v, want the CORRECT newest two [%s,%s]", items1, e5, e4)
	}
	cursor, _ := m1["next_cursor"].(string)
	if cursor != e4 {
		t.Fatalf("page1 next_cursor = %q, want e4's id %q", cursor, e4)
	}

	_, b2 := call(t, s, http.MethodGet, auditLogPath+"?limit=2&cursor="+cursor, "")
	m2 := decode(t, b2)
	items2, _ := m2["items"].([]any)
	// The DEFECT: a correct page 2 would be [e3, e2] (the next two newest
	// after e4). The real predicate instead yields exactly [e2] here -
	// proving e3 (and e1) are unreachable through this cursor, not merely
	// reordered.
	if len(items2) != 1 || items2[0].(map[string]any)["id"] != e2 {
		t.Fatalf("page2 = %v, want the WRONG (but faithfully reproduced) result [%s] - if this now returns [%s,%s], the defect has been silently fixed", items2, e2, e3, e2)
	}
	if _, has := m2["next_cursor"]; has {
		t.Fatalf("page2 (1 item, short of limit=2) carries next_cursor = %v, want ABSENT", m2["next_cursor"])
	}
	// e1/e3 exist and are real events - just unreachable via this cursor,
	// which is the point being documented (not asserted further: the whole
	// test IS that assertion).
	_ = e1
	_ = e3
}
