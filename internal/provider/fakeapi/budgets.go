package fakeapi

// Billing-budget route of the fake, reproducing what aether-api DOES
// (measured at aether-api 8e9045a; see the SDD task-12 contract,
// .superpowers/sdd/2026-09-17-terraform-provider-aether/task-12-measured-contract.md),
// NOT what its OpenAPI spec says:
//
//	GET    /organizations/{org}/billing/budget   200 Budget | 404
//	PUT    /organizations/{org}/billing/budget   200 Budget | 400 | 422 | 500
//	DELETE /organizations/{org}/billing/budget   204 | 404
//
// The budget is a true per-org SINGLETON row (org_id is the table's primary
// key, M028:1) - there is no separate id and no collection endpoint.
//
// Behaviours that matter to a Terraform provider and that the fake copies
// (contract section numbers below):
//   - a never-PUT org's GET is a 404 not_found "No budget set" - NEVER a 200
//     with zeros (§2). This is the one contract respect in which budgets
//     differ from cluster-network's "always-exists" singleton (networks.go);
//   - PUT's body is EXACTLY {monthly_limit_eur, thresholds_pct}: an unknown
//     top-level key, a fractional or out-of-int32 threshold, or a string
//     limit is a 400 bad_request "Invalid request body" (strict decode
//     straight into the wire types - a non-pointer float64/[]int32 pair, so
//     an absent key decodes to the same Go zero value Validate() treats as
//     invalid, exactly like the real SetBudgetRequest, §3);
//   - Validate() 422s (`validation_error`, details[] of {field,code,message}):
//     limit <= 0 (the zero value included) -> monthly_limit_eur/invalid/
//     "must be > 0"; an omitted, null, or empty thresholds_pct ->
//     thresholds_pct/required/"at least one threshold"; otherwise the
//     thresholds are checked ONE AT A TIME and stop at the first bad
//     element - out of (0,100] -> thresholds_pct/invalid/"each threshold
//     must be 0<pct<=100", not strictly greater than its predecessor
//     (covers both non-ascending order AND an exact duplicate) ->
//     thresholds_pct/invalid/"thresholds must be strictly ascending" - so at
//     MOST one thresholds_pct detail ever appears, but a limit detail and a
//     thresholds detail CAN appear together in the same 422 (§3);
//   - two validation holes 500 internal_error "Failed to save budget" (only
//     reached once Validate() found nothing to reject): a sub-half-cent
//     limit (0 < limit < 0.005) rounds to 0.00 in NUMERIC(12,2) and violates
//     its own CHECK > 0; a limit >= 9999999999.995 overflows NUMERIC(12,2)
//     (§3);
//   - upsert is a FULL REPLACE of both fields, 200 on both the very first
//     PUT and every later one (never 201); created_at survives a re-PUT
//     unchanged; updated_at moves on EVERY successful PUT, including a
//     byte-identical re-PUT; the stored limit is rounded to cents exactly
//     like NUMERIC(12,2) (math.Round(v*100)/100); thresholds are stored and
//     returned VERBATIM - never sorted or de-duplicated, because only a
//     strictly-ascending list is ever accepted (§3);
//   - DELETE is 204 and NOT idempotent: a second DELETE, or one against a
//     budget that was never set, is 404 "No budget set" - the identical
//     shape to GET's absent-budget 404 (§4). Only the budgets row is
//     deleted; there is nothing in this fake corresponding to
//     budget_alert_state, since no route here ever reads it (§4/§9);
//   - keyed on the EXISTING org store via the shared orgAccess helper: an
//     unknown org is 404, a foreign one (AddForeignOrg) is 403 - same as
//     organizations.go/webhooks.go/registry.go (§6).

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sync"
	"time"
)

var budgetPathRE = regexp.MustCompile(`^/organizations/([^/]+)/billing/budget$`)

// fakeBudget is one org's stored budget row. It exists only after the first
// successful PUT; before that, GET answers the §2 404 without ever creating
// a row (mirrors the store's pgx.ErrNoRows path).
type fakeBudget struct {
	limit      float64 // always already rounded to cents (math.Round(v*100)/100)
	thresholds []int32 // stored and echoed verbatim, in request order
	createdSeq int     // never changes after the row is created
	updatedSeq int     // bumps on EVERY successful PUT, identical body or not
}

// budgetStore is embedded by value in Server; its zero value is usable.
type budgetStore struct {
	mu    sync.Mutex
	byOrg map[string]*fakeBudget
	seq   int // monotonic clock shared by created_at/updated_at, like webhookStore.seq
}

func (bs *budgetStore) init() {
	if bs.byOrg == nil {
		bs.byOrg = map[string]*fakeBudget{}
	}
}

// budgetEpoch anchors created_at/updated_at: only relative movement and
// created_at == updated_at at creation matter to a caller, never this value.
var budgetEpoch = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

func budgetTime(seq int) string {
	return budgetEpoch.Add(time.Duration(seq) * time.Second).Format(time.RFC3339)
}

// roundToCents mirrors NUMERIC(12,2) exactly as T12-7 specifies.
func roundToCents(v float64) float64 { return math.Round(v*100) / 100 }

// budgetJSON renders the Budget schema for a stored row.
func budgetJSON(org string, b *fakeBudget) map[string]any {
	return map[string]any{
		"org_id":            org,
		"monthly_limit_eur": b.limit,
		"thresholds_pct":    append([]int32{}, b.thresholds...),
		"created_at":        budgetTime(b.createdSeq),
		"updated_at":        budgetTime(b.updatedSeq),
	}
}

// SeedBudget adds a budget out of band (no request is recorded, no
// validation applied - it can store what the real API would refuse, such as
// a non-ascending thresholds list) and returns the org id it was seeded
// under, for symmetry with the other fakes' Seed* knobs. thresholds values
// must fit in int32 (the wire type, matching a real PUT/echo) - a value
// outside that range panics rather than being silently truncated.
func (s *Server) SeedBudget(org string, limit float64, thresholds []int) string {
	bs := &s.budgets
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.init()
	bs.seq++
	t := make([]int32, len(thresholds))
	for i, v := range thresholds {
		if v < math.MinInt32 || v > math.MaxInt32 {
			panic(fmt.Sprintf("SeedBudget: threshold %d at index %d is out of int32 range", v, i))
		}
		t[i] = int32(v)
	}
	bs.byOrg[org] = &fakeBudget{limit: roundToCents(limit), thresholds: t, createdSeq: bs.seq, updatedSeq: bs.seq}
	return org
}

// RemoveBudget deletes a budget out of band (no request is recorded), as a
// portal deletion or a test simulating "removed elsewhere" needs.
func (s *Server) RemoveBudget(org string) {
	bs := &s.budgets
	bs.mu.Lock()
	defer bs.mu.Unlock()
	delete(bs.byOrg, org)
}

// routeBudget serves the billing-budget route; it reports false when path is
// not it.
func (s *Server) routeBudget(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	m := budgetPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	org := m[1]
	if !s.orgAccess(w, org) {
		return true
	}
	bs := &s.budgets
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.init()
	switch r.Method {
	case http.MethodGet:
		bs.get(w, org)
	case http.MethodPut:
		bs.put(w, org, body)
	case http.MethodDelete:
		bs.delete(w, org)
	default:
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
	}
	return true
}

// get is GET .../billing/budget (§2): 404 "No budget set" until the first
// successful PUT, the stored row afterwards.
func (bs *budgetStore) get(w http.ResponseWriter, org string) {
	b, ok := bs.byOrg[org]
	if !ok {
		apiError(w, http.StatusNotFound, "not_found", "No budget set")
		return
	}
	writeJSON(w, http.StatusOK, budgetJSON(org, b))
}

// put is PUT .../billing/budget (§3): strict decode, then Validate(), then
// the two 500 holes, then a full-replace upsert.
func (bs *budgetStore) put(w http.ResponseWriter, org string, body []byte) {
	// Non-pointer fields, exactly like the real SetBudgetRequest (BM:21-24):
	// an absent key decodes to the same Go zero value Validate() treats as
	// invalid, and a fractional/out-of-int32 threshold or a string limit
	// fails to decode at all - the strict-decode 400, never a 422 (§3).
	var req struct {
		MonthlyLimitEur float64 `json:"monthly_limit_eur"`
		ThresholdsPct   []int32 `json:"thresholds_pct"`
	}
	if !decodeStrict(body, &req) {
		apiError(w, http.StatusBadRequest, "bad_request", "Invalid request body")
		return
	}

	var details []map[string]any
	if req.MonthlyLimitEur <= 0 {
		details = append(details, detail("monthly_limit_eur", "invalid", "must be > 0"))
	}
	if len(req.ThresholdsPct) == 0 {
		details = append(details, detail("thresholds_pct", "required", "at least one threshold"))
	} else {
		for i, p := range req.ThresholdsPct {
			if p <= 0 || p > 100 {
				details = append(details, detail("thresholds_pct", "invalid", "each threshold must be 0<pct<=100"))
				break
			}
			if i > 0 && p <= req.ThresholdsPct[i-1] {
				details = append(details, detail("thresholds_pct", "invalid", "thresholds must be strictly ascending"))
				break
			}
		}
	}
	if details != nil {
		validationError(w, details)
		return
	}

	// Validation holes (§3): only reachable once Validate() found nothing to
	// reject, i.e. limit is already known > 0 here.
	if req.MonthlyLimitEur < 0.005 || req.MonthlyLimitEur >= 9999999999.995 {
		apiError(w, http.StatusInternalServerError, "internal_error", "Failed to save budget")
		return
	}

	// One seq increment per PUT: on create it stamps BOTH createdSeq and
	// updatedSeq with the same value (one now() default for both, contract
	// §1) - never two separate increments, which would make them differ from
	// the very first PUT.
	row, ok := bs.byOrg[org]
	bs.seq++
	if !ok {
		row = &fakeBudget{createdSeq: bs.seq} // set once; never touched again (survives every re-PUT)
		bs.byOrg[org] = row
	}
	row.limit = roundToCents(req.MonthlyLimitEur)
	row.thresholds = append([]int32(nil), req.ThresholdsPct...)
	row.updatedSeq = bs.seq // moves on EVERY successful PUT, identical body included (§3)
	writeJSON(w, http.StatusOK, budgetJSON(org, row))
}

// delete is DELETE .../billing/budget (§4): 204, then NOT idempotent - a
// second DELETE (or one that was never preceded by any PUT) is 404.
func (bs *budgetStore) delete(w http.ResponseWriter, org string) {
	if _, ok := bs.byOrg[org]; !ok {
		apiError(w, http.StatusNotFound, "not_found", "No budget set")
		return
	}
	delete(bs.byOrg, org)
	w.WriteHeader(http.StatusNoContent)
}
