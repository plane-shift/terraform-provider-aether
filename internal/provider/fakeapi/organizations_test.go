package fakeapi

import (
	"net/http"
	"testing"
)

const orgPath = "/organizations/" + testOrg

const foreignOrg = "22222222-2222-4222-8222-222222222222"

func putOrg(t *testing.T, s *Server, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodPut, orgPath, body)
	return resp, decode(t, b)
}

// Fails if billing_email/vat_id appear before anything wrote them, if plan is
// missing or not "starter", or if a key the client decodes is dropped.
func TestOrgGetOmitsNullBillingFieldsAndAlwaysCarriesPlan(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodGet, orgPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	for _, k := range []string{"billing_email", "vat_id"} {
		if _, has := m[k]; has {
			t.Fatalf("%s present while NULL: %s (must be absent, §2)", k, b)
		}
	}
	if m["plan"] != "starter" || m["name"] != "Acme" || m["slug"] != "acme" || m["id"] != testOrg {
		t.Fatalf("body = %s", b)
	}
	if m["created_at"] == nil || m["updated_at"] == nil {
		t.Fatalf("timestamps missing: %s", b)
	}
}

// Fails if an omitted or null key overwrites the stored value (a replace
// instead of COALESCE-keep), or if a key that was sent is ignored.
func TestOrgPutIsACoalesceMergeOmittedAndNullKeep(t *testing.T) {
	s := newClusterFake(t)
	putOrg(t, s, `{"name":"Acme 2","billing_email":"a@x.io","vat_id":"RO1"}`)
	_, m := putOrg(t, s, `{"name":"Acme 3"}`)
	if m["name"] != "Acme 3" || m["billing_email"] != "a@x.io" || m["vat_id"] != "RO1" || m["slug"] != "acme" {
		t.Fatalf("omitted keys were not kept: %v", m)
	}
	_, m = putOrg(t, s, `{"billing_email":null,"vat_id":null,"name":null}`)
	if m["name"] != "Acme 3" || m["billing_email"] != "a@x.io" || m["vat_id"] != "RO1" {
		t.Fatalf("explicit null must KEEP, got %v", m)
	}
}

// Fails if "" is dropped/cleared to absent instead of stored and echoed as "",
// or if the emptiness does not persist to a later GET.
func TestOrgPutEmptyStringIsStoredAndReadsBackEmptyNeverAbsent(t *testing.T) {
	s := newClusterFake(t)
	putOrg(t, s, `{"billing_email":"a@x.io","vat_id":"RO1"}`)
	_, m := putOrg(t, s, `{"billing_email":"","vat_id":""}`)
	be, hasBE := m["billing_email"]
	vat, hasVat := m["vat_id"]
	if !hasBE || be != "" || !hasVat || vat != "" {
		t.Fatalf("after a clear, both keys must be present as \"\": %v", m)
	}
	_, b := call(t, s, http.MethodGet, orgPath, "")
	g := decode(t, b)
	if v, has := g["billing_email"]; !has || v != "" {
		t.Fatalf("a later GET lost the stored \"\": %s", b)
	}
	// name and slug take "" too: the API validates neither (§3).
	if resp, m := putOrg(t, s, `{"name":"","slug":""}`); resp.StatusCode != http.StatusOK || m["name"] != "" || m["slug"] != "" {
		t.Fatalf("PUT name/slug \"\" = %d %v, want 200 and stored", resp.StatusCode, m)
	}
}

// Fails if updated_at stays put on a PUT (including the empty object).
func TestOrgPutBumpsUpdatedAtOnEveryPUTEvenEmpty(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodGet, orgPath, "")
	u0 := decode(t, b)["updated_at"]
	_, m1 := putOrg(t, s, `{}`)
	_, m2 := putOrg(t, s, `{}`)
	if m1["updated_at"] == u0 || m2["updated_at"] == m1["updated_at"] {
		t.Fatalf("updated_at %v -> %v -> %v: must move on every PUT", u0, m1["updated_at"], m2["updated_at"])
	}
	if m1["created_at"] != m2["created_at"] {
		t.Fatalf("created_at moved")
	}
}

// Fails if a duplicate slug is accepted, uses another status, or if the
// failed PUT still changed the name (it must be atomic).
func TestOrgPutDuplicateSlugIs409AndChangesNothing(t *testing.T) {
	s := newClusterFake(t)
	s.AddOrg("33333333-3333-4333-8333-333333333333", "Other", "other")
	resp, m := putOrg(t, s, `{"name":"Renamed","slug":"other"}`)
	if resp.StatusCode != http.StatusConflict || errCode(m) != "conflict" || m["error"].(map[string]any)["message"] != "Organization already exists" {
		t.Fatalf("PUT = %d %v, want 409 conflict \"Organization already exists\"", resp.StatusCode, m)
	}
	_, b := call(t, s, http.MethodGet, orgPath, "")
	if g := decode(t, b); g["name"] != "Acme" || g["slug"] != "acme" {
		t.Fatalf("a refused PUT changed the org: %s", b)
	}
	// re-sending its own slug is not a duplicate
	if resp, _ := putOrg(t, s, `{"slug":"acme"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("own slug = %d, want 200", resp.StatusCode)
	}
}

// Fails if an unknown key is tolerated (the real decoder is strict).
func TestOrgPutUnknownKeyIs400(t *testing.T) {
	s := newClusterFake(t)
	resp, m := putOrg(t, s, `{"name":"x","plan":"enterprise"}`)
	if resp.StatusCode != http.StatusBadRequest || errCode(m) != "bad_request" {
		t.Fatalf("PUT = %d %v, want 400 (plan is not writable)", resp.StatusCode, m)
	}
}

// Fails if a foreign org answers 404 (or 200), or if any org route leaks it;
// an unregistered id must stay 404 (Configure's tests depend on it), and the
// list must hide the foreign org.
func TestForeignOrgIs403OnEveryRouteAndUnknownStays404(t *testing.T) {
	s := newClusterFake(t)
	s.AddForeignOrg(foreignOrg, "Them", "them")
	p := "/organizations/" + foreignOrg
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, p, ""}, {http.MethodPut, p, `{}`},
		{http.MethodGet, p + "/members", ""}, {http.MethodPost, p + "/members", `{"email":"a@x","role":"viewer"}`},
		{http.MethodGet, p + "/members/" + foreignOrg, ""}, {http.MethodDelete, p + "/members/" + foreignOrg, ""},
	} {
		resp, b := call(t, s, c.method, c.path, c.body)
		m := decode(t, b)
		if resp.StatusCode != http.StatusForbidden || errCode(m) != "forbidden" ||
			m["error"].(map[string]any)["message"] != "You do not have access to this organization" {
			t.Fatalf("%s %s = %d %s, want 403 forbidden \"You do not have access to this organization\"", c.method, c.path, resp.StatusCode, b)
		}
	}
	if resp, _ := call(t, s, http.MethodGet, "/organizations/44444444-4444-4444-8444-444444444444", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unregistered org = %d, want 404", resp.StatusCode)
	}
	_, b := call(t, s, http.MethodGet, "/organizations", "")
	if items := decode(t, b)["items"].([]any); len(items) != 1 {
		t.Fatalf("list = %s, want only the reachable org", b)
	}
}

// Fails if DELETE by a token is allowed (the provider never calls it).
func TestOrgDeleteByATokenIs403(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodDelete, orgPath, "")
	if resp.StatusCode != http.StatusForbidden || errCode(decode(t, b)) != "token_not_permitted" {
		t.Fatalf("DELETE = %d %s, want 403 token_not_permitted", resp.StatusCode, b)
	}
}

// Fails if SetOrgBilling does not surface through GET (the resource tests
// seed imports with it).
func TestSetOrgBillingSeedsAbsentVersusEmpty(t *testing.T) {
	s := newClusterFake(t)
	e := ""
	s.SetOrgBilling(testOrg, nil, &e)
	_, b := call(t, s, http.MethodGet, orgPath, "")
	m := decode(t, b)
	if _, has := m["billing_email"]; has {
		t.Fatalf("billing_email must stay absent: %s", b)
	}
	if v, has := m["vat_id"]; !has || v != "" {
		t.Fatalf("vat_id must be present as \"\": %s", b)
	}
}
