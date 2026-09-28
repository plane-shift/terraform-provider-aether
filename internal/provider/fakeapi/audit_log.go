package fakeapi

// Audit-log route of the fake, reproducing what aether-api DOES (measured at
// aether-api 8e9045a; SDD task-14-measured-contract.md §3), not what its
// OpenAPI spec says:
//
//	GET /organizations/{org}/audit-log[?cursor=&limit=]   200 {items[],next_cursor?}
//
// Behaviours that matter and that the fake copies:
//   - NO filters exist server-side - only cursor/limit are ever read (§3);
//   - `limit` is 1..100, default 50; anything outside that range, or
//     unparseable, silently becomes 50 (§3 - NOT clamped to the nearest
//     bound, replaced outright);
//   - true order is `created_at DESC, id DESC` (newest first, §3);
//   - the cursor is BROKEN: page 2+ filters with a bare `id < cursor`
//     predicate that has nothing to do with created_at, over RANDOM v4
//     UUIDs (§3). brokenCursorPage (shared with billing_reads.go's invoice
//     list, which has the identical defect) reproduces exactly that: filter
//     first by id, THEN sort the filtered set by the true order, THEN
//     truncate to limit - matching a SQL `WHERE id < cursor ORDER BY
//     created_at DESC, id DESC LIMIT n` row for row. Page 1 (cursor=="") is
//     unaffected and is always correct;
//   - `next_cursor` is the returned page's LAST item's id, present only when
//     the page is exactly `limit` items long (AH:51-55) - never present on a
//     short or empty page;
//   - `metadata` is emitted only when the seed's Metadata is non-nil (SQL
//     NULL is "absent", not JSON null, §3);
//   - `actor_id`/`actor_email`/`resource_id`/`ip_address` are each omitted
//     when their seed pointer is nil (§3 - actor_email in particular only
//     ever exists for actor_type "user").

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

var auditLogPathRE = regexp.MustCompile(`^/organizations/([^/]+)/audit-log$`)

// AuditEventSeed is one event to seed via SeedAuditEvent. ID is generated
// when empty; CreatedAt defaults to an auto-incrementing clock (one second
// per seeded event, in call order) when zero - set both explicitly to
// control the id-order-vs-time-order divergence the broken-cursor unit test
// needs.
type AuditEventSeed struct {
	ID                                         string
	ActorID, ActorEmail, ResourceID, IPAddress *string
	ActorType, Action, ResourceType            string
	Metadata                                   any // nil = SQL NULL = omitted on the wire
	CreatedAt                                  time.Time
}

type fakeAuditEvent struct {
	id, orgID                       string
	actorID, actorEmail             *string
	actorType, action, resourceType string
	resourceID                      *string
	metadata                        any
	ipAddress                       *string
	createdAt                       time.Time
}

// auditLogStore is embedded by value in Server; its zero value is usable.
type auditLogStore struct {
	mu    sync.Mutex
	byOrg map[string][]*fakeAuditEvent
	seq   int
}

// auditEpoch anchors auto-assigned CreatedAt values when a seed omits one.
var auditEpoch = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

// SeedAuditEvent adds an audit event out of band (no request is recorded, no
// validation) and returns its id.
func (s *Server) SeedAuditEvent(org string, e AuditEventSeed) string {
	al := &s.auditLog
	al.mu.Lock()
	defer al.mu.Unlock()
	if al.byOrg == nil {
		al.byOrg = map[string][]*fakeAuditEvent{}
	}
	al.seq++
	id := e.ID
	if id == "" {
		id = uuid.NewString()
	}
	created := e.CreatedAt
	if created.IsZero() {
		created = auditEpoch.Add(time.Duration(al.seq) * time.Second)
	}
	ev := &fakeAuditEvent{
		id: id, orgID: org, actorID: e.ActorID, actorEmail: e.ActorEmail,
		actorType: e.ActorType, action: e.Action, resourceType: e.ResourceType,
		resourceID: e.ResourceID, metadata: e.Metadata, ipAddress: e.IPAddress,
		createdAt: created,
	}
	al.byOrg[org] = append(al.byOrg[org], ev)
	return id
}

func auditEventJSON(e *fakeAuditEvent) map[string]any {
	m := map[string]any{
		"id": e.id, "org_id": e.orgID, "actor_type": e.actorType,
		"action": e.action, "resource_type": e.resourceType,
		"created_at": e.createdAt.Format(time.RFC3339),
	}
	if e.actorID != nil {
		m["actor_id"] = *e.actorID
	}
	if e.actorEmail != nil {
		m["actor_email"] = *e.actorEmail
	}
	if e.resourceID != nil {
		m["resource_id"] = *e.resourceID
	}
	if e.metadata != nil {
		m["metadata"] = e.metadata
	}
	if e.ipAddress != nil {
		m["ip_address"] = *e.ipAddress
	}
	return m
}

// auditLimit mirrors parsePagination's limit rule (§3): anything outside
// 1..100, or unparseable, becomes exactly 50 - not clamped to the nearest
// bound.
func auditLimit(raw string) int {
	if raw == "" {
		return 50
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 100 {
		return 50
	}
	return n
}

// brokenCursorPage reproduces the shared defect of the audit-log and invoice
// list routes (contract §3/§6, aether-api issue A): a keyset cursor SHOULD
// filter and order on the same key, but the real query filters on `id <
// cursor` while ordering on an unrelated column. id extracts an item's id,
// less reports the TRUE intended order (a "less(i,j)" that returns whether i
// sorts before j - e.g. "created_at DESC, id DESC"). Semantics: filter ALL of
// all by "id < cursor" (or no filter at all when cursor==""), THEN sort the
// filtered set by less, THEN truncate to limit - exactly the row-by-row
// result of `WHERE id < cursor ORDER BY <less> LIMIT n` against the full set,
// cursor filter and true order deliberately out of sync. next is the last
// returned item's id, present only when the page came back exactly `limit`
// items long.
func brokenCursorPage[T any](all []T, id func(T) string, less func(a, b T) bool, cursor string, limit int) (page []T, next string) {
	var filtered []T
	for _, x := range all {
		if cursor == "" || id(x) < cursor {
			filtered = append(filtered, x)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return less(filtered[i], filtered[j]) })
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	if len(filtered) == limit && limit > 0 {
		next = id(filtered[len(filtered)-1])
	}
	return filtered, next
}

// routeAuditLog serves the audit-log route; it reports false when path is
// not it.
func (s *Server) routeAuditLog(w http.ResponseWriter, r *http.Request, path string) bool {
	m := auditLogPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	org := m[1]
	if !s.orgAccess(w, org) {
		return true
	}
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		return true
	}
	al := &s.auditLog
	al.mu.Lock()
	all := append([]*fakeAuditEvent(nil), al.byOrg[org]...)
	al.mu.Unlock()
	limit := auditLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	less := func(a, b *fakeAuditEvent) bool {
		if !a.createdAt.Equal(b.createdAt) {
			return a.createdAt.After(b.createdAt) // created_at DESC
		}
		return a.id > b.id // id DESC tiebreak
	}
	page, next := brokenCursorPage(all, func(e *fakeAuditEvent) string { return e.id }, less, cursor, limit)
	items := make([]map[string]any, 0, len(page))
	for _, e := range page {
		items = append(items, auditEventJSON(e))
	}
	out := map[string]any{"items": items}
	if next != "" {
		out["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, out)
	return true
}
