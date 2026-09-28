package fakeapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// These are the fake's own tests (task-12-brief-amended.md test 11):
// budgets.go must reproduce the measured aether-api contract exactly (see
// task-12-measured-contract.md), independent of anything the resource
// (resource_billing_budget.go, not yet written) does with it.

const budgetPath = "/organizations/" + testOrg + "/billing/budget"

func newBudgetFake(t *testing.T) *Server {
	t.Helper()
	s := New(t)
	s.AddOrg(testOrg, "Acme", "acme")
	return s
}

func getBudget(t *testing.T, s *Server) (*http.Response, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodGet, budgetPath, "")
	return resp, decode(t, b)
}

func putBudget(t *testing.T, s *Server, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodPut, budgetPath, body)
	return resp, decode(t, b)
}

// ---------------------------------------------------------------- §2

// Fails if the fake ever answers 200 (with zeros or otherwise) before any
// PUT, uses the wrong error code, or drops the X-Request-ID header every
// other error route carries.
func TestBudgetGetBeforeAnyPUTIs404NoBudgetSet(t *testing.T) {
	s := newBudgetFake(t)
	resp, m := getBudget(t, s)
	if resp.StatusCode != http.StatusNotFound || errCode(m) != "not_found" {
		t.Fatalf("GET before any PUT = %d %v, want 404 not_found", resp.StatusCode, m)
	}
	if got := m["error"].(map[string]any)["message"]; got != "No budget set" {
		t.Fatalf("message = %q, want %q", got, "No budget set")
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatalf("no X-Request-ID response header on a 404")
	}
}

// ---------------------------------------------------------------- §3

// Fails if the fake normalises/sorts/de-dupes the thresholds list, fails to
// echo the limit rounded to cents, or fails to bump updated_at on a
// byte-identical re-PUT while keeping created_at stable.
func TestBudgetPutIsFullReplaceAndBumpsUpdatedAtOnAnIdenticalRePUT(t *testing.T) {
	s := newBudgetFake(t)
	body := `{"monthly_limit_eur":100,"thresholds_pct":[50,80,100]}`
	resp, m := putBudget(t, s, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %v, want 200", resp.StatusCode, m)
	}
	if m["org_id"] != testOrg {
		t.Fatalf("org_id = %v, want %q", m["org_id"], testOrg)
	}
	if numF(m, "monthly_limit_eur") != 100 {
		t.Fatalf("monthly_limit_eur = %v, want 100", m["monthly_limit_eur"])
	}
	th, ok := m["thresholds_pct"].([]any)
	if !ok || len(th) != 3 || num2(th[0]) != 50 || num2(th[1]) != 80 || num2(th[2]) != 100 {
		t.Fatalf("thresholds_pct = %v, want [50,80,100] verbatim in order", m["thresholds_pct"])
	}
	created1, _ := m["created_at"].(string)
	updated1, _ := m["updated_at"].(string)
	if created1 == "" || updated1 == "" {
		t.Fatalf("created_at/updated_at = %q/%q, want both set on the first PUT", created1, updated1)
	}

	// A byte-identical re-PUT: created_at stays put, updated_at still moves
	// (§3: "even an identical one").
	resp2, m2 := putBudget(t, s, body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second PUT = %d %v, want 200 (never 201 - upsert)", resp2.StatusCode, m2)
	}
	if got := m2["created_at"]; got != created1 {
		t.Fatalf("created_at changed across a re-PUT: got %v, want unchanged %q", got, created1)
	}
	if got := m2["updated_at"]; got == updated1 {
		t.Fatalf("updated_at stayed %q across a second, byte-identical PUT; it must move on every PUT", got)
	}

	// And a plain GET afterwards reads back exactly what the PUT answered.
	respG, mG := getBudget(t, s)
	if respG.StatusCode != http.StatusOK || mG["updated_at"] != m2["updated_at"] {
		t.Fatalf("GET after PUT = %d %v, want the same row the PUT just returned", respG.StatusCode, mG)
	}
}

// Fails if the fake stores the raw float instead of rounding to cents like
// NUMERIC(12,2), in either direction (truncating instead of rounding, or
// rounding the wrong way).
func TestBudgetPutRoundsLimitToCentsLikeNumeric12_2(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{100, 100},
		{12.3, 12.3},
		{0.01, 0.01},
		{12.345, 12.35}, // rounds up
		{12.339, 12.34}, // 3rd decimal rounds the 2nd down... here up
		{9999999999.99, 9999999999.99},
		{0.005, 0.01}, // exactly the 0.005 boundary is NOT the <0.005 500 hole
	}
	for _, c := range cases {
		s := newBudgetFake(t)
		resp, m := putBudget(t, s, budgetBody(c.in, []int{50}))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT limit=%v = %d %v, want 200", c.in, resp.StatusCode, m)
		}
		if got := numF(m, "monthly_limit_eur"); got != c.want {
			t.Fatalf("PUT limit=%v -> monthly_limit_eur = %v, want %v (NUMERIC(12,2) rounding)", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------- §1: 400

// Fails if the fake accepts (or mis-codes) an unknown top-level key, a
// fractional/out-of-int32 threshold, or a string limit instead of the
// strict-decode 400 every other write route uses.
func TestBudgetPutMalformedBodyIs400InvalidRequestBody(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"unknown key", `{"monthly_limit_eur":100,"thresholds_pct":[50],"bogus":1}`},
		{"fractional threshold", `{"monthly_limit_eur":100,"thresholds_pct":[50.5]}`},
		{"threshold above int32", `{"monthly_limit_eur":100,"thresholds_pct":[2147483648]}`},
		{"string limit", `{"monthly_limit_eur":"100","thresholds_pct":[50]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newBudgetFake(t)
			resp, m := putBudget(t, s, c.body)
			if resp.StatusCode != http.StatusBadRequest || errCode(m) != "bad_request" {
				t.Fatalf("PUT %s = %d %v, want 400 bad_request", c.body, resp.StatusCode, m)
			}
			if got := m["error"].(map[string]any)["message"]; got != "Invalid request body" {
				t.Fatalf("message = %q, want %q", got, "Invalid request body")
			}
		})
	}
}

// ---------------------------------------------------------------- §3: 422

// Fails if any of the four detail shapes drift from the exact
// field/code/message the real API returns, if more than one thresholds_pct
// detail is ever produced, or if a limit detail and a thresholds detail
// cannot appear TOGETHER in the same response.
func TestBudgetPutValidationIs422WithExactFieldCodeAndMessage(t *testing.T) {
	cases := []struct {
		name, body                      string
		wantFields, wantCodes, wantMsgs []string
	}{
		{"limit zero", `{"monthly_limit_eur":0,"thresholds_pct":[50]}`,
			[]string{"monthly_limit_eur"}, []string{"invalid"}, []string{"must be > 0"}},
		{"limit omitted (decodes to the zero value)", `{"thresholds_pct":[50]}`,
			[]string{"monthly_limit_eur"}, []string{"invalid"}, []string{"must be > 0"}},
		{"limit negative", `{"monthly_limit_eur":-5,"thresholds_pct":[50]}`,
			[]string{"monthly_limit_eur"}, []string{"invalid"}, []string{"must be > 0"}},
		{"thresholds omitted", `{"monthly_limit_eur":100}`,
			[]string{"thresholds_pct"}, []string{"required"}, []string{"at least one threshold"}},
		{"thresholds null", `{"monthly_limit_eur":100,"thresholds_pct":null}`,
			[]string{"thresholds_pct"}, []string{"required"}, []string{"at least one threshold"}},
		{"thresholds empty", `{"monthly_limit_eur":100,"thresholds_pct":[]}`,
			[]string{"thresholds_pct"}, []string{"required"}, []string{"at least one threshold"}},
		{"threshold zero", `{"monthly_limit_eur":100,"thresholds_pct":[0]}`,
			[]string{"thresholds_pct"}, []string{"invalid"}, []string{"each threshold must be 0<pct<=100"}},
		{"threshold above 100", `{"monthly_limit_eur":100,"thresholds_pct":[50,101]}`,
			[]string{"thresholds_pct"}, []string{"invalid"}, []string{"each threshold must be 0<pct<=100"}},
		{"non-ascending", `{"monthly_limit_eur":100,"thresholds_pct":[80,50]}`,
			[]string{"thresholds_pct"}, []string{"invalid"}, []string{"thresholds must be strictly ascending"}},
		{"duplicate", `{"monthly_limit_eur":100,"thresholds_pct":[50,50]}`,
			[]string{"thresholds_pct"}, []string{"invalid"}, []string{"thresholds must be strictly ascending"}},
		{"limit AND thresholds both invalid at once", `{"monthly_limit_eur":0,"thresholds_pct":[]}`,
			[]string{"monthly_limit_eur", "thresholds_pct"}, []string{"invalid", "required"},
			[]string{"must be > 0", "at least one threshold"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newBudgetFake(t)
			resp, m := putBudget(t, s, c.body)
			if resp.StatusCode != http.StatusUnprocessableEntity || errCode(m) != "validation_error" {
				t.Fatalf("PUT %s = %d %v, want 422 validation_error", c.body, resp.StatusCode, m)
			}
			details, _ := m["error"].(map[string]any)["details"].([]any)
			if len(details) != len(c.wantFields) {
				t.Fatalf("details = %v, want exactly %d entr(ies)", details, len(c.wantFields))
			}
			for i := range c.wantFields {
				dm := details[i].(map[string]any)
				if dm["field"] != c.wantFields[i] || dm["code"] != c.wantCodes[i] || dm["message"] != c.wantMsgs[i] {
					t.Fatalf("details[%d] = %v, want {field:%q code:%q message:%q}", i, dm, c.wantFields[i], c.wantCodes[i], c.wantMsgs[i])
				}
			}
		})
	}
}

// Fails if the per-element threshold loop keeps checking after the first bad
// element (producing more than one thresholds_pct detail) instead of
// breaking, e.g. a list with BOTH an out-of-range entry and a non-ascending
// pair must report only the first problem encountered.
func TestBudgetPutThresholdLoopStopsAtTheFirstBadElement(t *testing.T) {
	s := newBudgetFake(t)
	// index 0 (150) is out of range; index 2 (50) is also non-ascending vs
	// index 1 (200) - only the range problem at index 0 must be reported.
	resp, m := putBudget(t, s, `{"monthly_limit_eur":100,"thresholds_pct":[150,200,50]}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT = %d %v, want 422", resp.StatusCode, m)
	}
	details, _ := m["error"].(map[string]any)["details"].([]any)
	if len(details) != 1 {
		t.Fatalf("details = %v, want exactly 1 (break at the first bad element)", details)
	}
	dm := details[0].(map[string]any)
	if dm["message"] != "each threshold must be 0<pct<=100" {
		t.Fatalf("details[0] = %v, want the range message (index 0 is bad first)", dm)
	}
}

// ---------------------------------------------------------------- §3: 500s

// Fails if the fake stores (or 422s) a sub-half-cent limit or an
// overflowing one instead of the measured 500, or accepts either.
func TestBudgetPutSubCentOrOverflowLimitIs500FailedToSaveBudget(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"just under the half-cent floor", `{"monthly_limit_eur":0.001,"thresholds_pct":[50]}`},
		{"at the overflow boundary", `{"monthly_limit_eur":9999999999.995,"thresholds_pct":[50]}`},
		{"well past the overflow boundary", `{"monthly_limit_eur":10000000000,"thresholds_pct":[50]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newBudgetFake(t)
			resp, m := putBudget(t, s, c.body)
			if resp.StatusCode != http.StatusInternalServerError || errCode(m) != "internal_error" {
				t.Fatalf("PUT %s = %d %v, want 500 internal_error", c.body, resp.StatusCode, m)
			}
			if got := m["error"].(map[string]any)["message"]; got != "Failed to save budget" {
				t.Fatalf("message = %q, want %q", got, "Failed to save budget")
			}
		})
	}
	// And the row must not have been created by the failing PUT: a
	// never-set budget still reads 404, never a stored zero/garbage value.
	s := newBudgetFake(t)
	putBudget(t, s, `{"monthly_limit_eur":0.001,"thresholds_pct":[50]}`)
	resp, m := getBudget(t, s)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after a 500'd PUT = %d %v, want 404 (nothing was ever stored)", resp.StatusCode, m)
	}
}

// ---------------------------------------------------------------- §4

// Fails if DELETE is idempotent (a real defect the fake must NOT smooth
// over), if it answers anything but 204 on the first delete, or if it uses a
// different 404 shape than GET's.
func TestBudgetDeleteIs204ThenNotIdempotent(t *testing.T) {
	s := newBudgetFake(t)
	putBudget(t, s, `{"monthly_limit_eur":100,"thresholds_pct":[50]}`)
	resp, b := call(t, s, http.MethodDelete, budgetPath, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first DELETE = %d %s, want 204", resp.StatusCode, b)
	}
	if len(b) != 0 {
		t.Fatalf("204 body = %q, want empty", b)
	}
	resp2, b2 := call(t, s, http.MethodDelete, budgetPath, "")
	m2 := decode(t, b2)
	if resp2.StatusCode != http.StatusNotFound || errCode(m2) != "not_found" {
		t.Fatalf("second DELETE = %d %s, want 404 not_found (not idempotent)", resp2.StatusCode, b2)
	}
	if got := m2["error"].(map[string]any)["message"]; got != "No budget set" {
		t.Fatalf("second DELETE message = %q, want %q (same shape as GET's absent-budget 404)", got, "No budget set")
	}
	respG, mG := getBudget(t, s)
	if respG.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE = %d %v, want 404 (the row is really gone)", respG.StatusCode, mG)
	}
}

// A DELETE with no budget ever set (no PUT at all) is the same 404 shape.
func TestBudgetDeleteWithNoBudgetEverSetIs404(t *testing.T) {
	s := newBudgetFake(t)
	resp, b := call(t, s, http.MethodDelete, budgetPath, "")
	m := decode(t, b)
	if resp.StatusCode != http.StatusNotFound || errCode(m) != "not_found" {
		t.Fatalf("DELETE with nothing ever set = %d %s, want 404 not_found", resp.StatusCode, b)
	}
}

// ---------------------------------------------------------------- §6

// Fails if the route stops keying off the shared org store (an unknown org
// must 404, a foreign one must 403 - the same shared orgAccess helper every
// other org-scoped route in this fake uses).
func TestBudgetRoutes404ForUnknownOrgAnd403ForForeignOrg(t *testing.T) {
	s := newBudgetFake(t)
	const foreignOrg = "22222222-2222-4222-8222-222222222222"
	s.AddForeignOrg(foreignOrg, "Other", "other")
	unknownOrg := "33333333-3333-4333-8333-333333333333"

	for _, path := range []string{
		"/organizations/" + unknownOrg + "/billing/budget",
	} {
		resp, b := call(t, s, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusNotFound || errCode(decode(t, b)) != "not_found" {
			t.Fatalf("GET %s = %d %s, want 404 not_found", path, resp.StatusCode, b)
		}
	}
	foreignPath := "/organizations/" + foreignOrg + "/billing/budget"
	resp, b := call(t, s, http.MethodGet, foreignPath, "")
	if resp.StatusCode != http.StatusForbidden || errCode(decode(t, b)) != "forbidden" {
		t.Fatalf("GET %s (foreign org) = %d %s, want 403 forbidden", foreignPath, resp.StatusCode, b)
	}
}

// ---------------------------------------------------------------- Seed/Remove knobs

// Fails if SeedBudget/RemoveBudget record a request, apply validation, or
// fail to round the seeded limit to cents like every PUT-derived row.
func TestSeedBudgetAndRemoveBudgetAreOutOfBandAndUnvalidated(t *testing.T) {
	s := newBudgetFake(t)
	before := len(s.Requests())
	// A non-ascending list the real PUT would 422 on - Seed must accept it
	// verbatim, unlike a PUT.
	s.SeedBudget(testOrg, 12.345, []int{80, 50})
	if got := len(s.Requests()); got != before {
		t.Fatalf("SeedBudget recorded %d request(s), want 0", got-before)
	}
	resp, m := getBudget(t, s)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET after SeedBudget = %d %v, want 200", resp.StatusCode, m)
	}
	if got := numF(m, "monthly_limit_eur"); got != 12.35 {
		t.Fatalf("seeded monthly_limit_eur = %v, want 12.35 (rounded to cents like a real PUT)", got)
	}
	th, _ := m["thresholds_pct"].([]any)
	if len(th) != 2 || num2(th[0]) != 80 || num2(th[1]) != 50 {
		t.Fatalf("seeded thresholds_pct = %v, want [80,50] verbatim (Seed applies no validation)", m["thresholds_pct"])
	}

	beforeRemove := len(s.Requests())
	s.RemoveBudget(testOrg)
	if got := len(s.Requests()); got != beforeRemove {
		t.Fatalf("RemoveBudget recorded %d request(s), want 0", got-beforeRemove)
	}
	resp2, m2 := getBudget(t, s)
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after RemoveBudget = %d %v, want 404", resp2.StatusCode, m2)
	}
}

// ---------------------------------------------------------------- helpers

// budgetBody renders a PUT body for a given limit and thresholds via
// encoding/json, so the limit's wire representation (which may use
// scientific notation for a very large value, e.g. 9999999999.99) round-
// trips through the fake's own strict decoder exactly like a real client's.
func budgetBody(limit float64, thresholds []int) string {
	b, err := json.Marshal(map[string]any{"monthly_limit_eur": limit, "thresholds_pct": thresholds})
	if err != nil {
		panic(err) // a fixed shape of Go values cannot fail to marshal
	}
	return string(b)
}

// numF reads a decoded JSON object field that is a number, as float64 (never
// truncated to int - unlike the package's num() helper, needed here because
// monthly_limit_eur carries cents).
func numF(m map[string]any, k string) float64 { return m[k].(float64) }

// num2 reads a decoded JSON array element that is itself a bare number
// (json.Unmarshal into interface{} always yields float64).
func num2(v any) int { return int(v.(float64)) }
