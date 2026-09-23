package fakeapi

import (
	"fmt"
	"net/http"
	"testing"
)

const membersPath = orgPath + "/members"

func postMember(t *testing.T, s *Server, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodPost, membersPath, body)
	return resp, decode(t, b)
}

func detailsOf(m map[string]any) []any { return m["error"].(map[string]any)["details"].([]any) }

// Fails if POST answers other than 201, echoes the wrong shape, or leaks
// invited_by/accepted_at (never written on API-created rows).
func TestMemberPostIs201WithTheExactKeySet(t *testing.T) {
	s := newClusterFake(t)
	resp, m := postMember(t, s, `{"email":"a@x.io","role":"admin"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST = %d %v, want 201", resp.StatusCode, m)
	}
	got := map[string]bool{}
	for k := range m {
		got[k] = true
	}
	for _, k := range []string{"id", "org_id", "email", "role", "created_at"} {
		if !got[k] {
			t.Fatalf("missing %s: %v", k, m)
		}
		delete(got, k)
	}
	if len(got) != 0 {
		t.Fatalf("extra keys %v in %v: invited_by/accepted_at must never appear", got, m)
	}
	if m["org_id"] != testOrg || m["email"] != "a@x.io" || m["role"] != "admin" {
		t.Fatalf("body = %v", m)
	}
}

// Fails if any of the three 422 shapes drifts from the measured field/code/text.
func TestMemberPostValidationDetails(t *testing.T) {
	for _, c := range []struct{ name, body, field, code, msg string }{
		{"no email", `{"role":"viewer"}`, "email", "required", "Email is required"},
		{"no role", `{"email":"a@x"}`, "role", "required", "Role is required"},
		{"bad role", `{"email":"a@x","role":"root"}`, "role", "invalid", "Role must be one of: owner, admin, member, viewer"},
		{"upper-case role", `{"email":"a@x","role":"Owner"}`, "role", "invalid", "Role must be one of: owner, admin, member, viewer"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newClusterFake(t)
			resp, m := postMember(t, s, c.body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d %v, want 422", resp.StatusCode, m)
			}
			d := detailsOf(m)
			if len(d) != 1 {
				t.Fatalf("details = %v, want exactly one", d)
			}
			f := d[0].(map[string]any)
			if f["field"] != c.field || f["code"] != c.code || f["message"] != c.msg {
				t.Fatalf("detail = %v, want %s/%s/%q", f, c.field, c.code, c.msg)
			}
		})
	}
	s := newClusterFake(t)
	if resp, m := postMember(t, s, `{"email":"a@x","role":"viewer","org_id":"x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown key = %d %v, want 400", resp.StatusCode, m)
	}
}

// Fails if uniqueness is case-insensitive (Alice@x and alice@x are two rows),
// or if an exact duplicate is anything but 409 conflict.
func TestMemberDuplicateIsExactCaseSensitive409(t *testing.T) {
	s := newClusterFake(t)
	postMember(t, s, `{"email":"alice@x.io","role":"member"}`)
	resp, m := postMember(t, s, `{"email":"alice@x.io","role":"admin"}`)
	if resp.StatusCode != http.StatusConflict || errCode(m) != "conflict" || m["error"].(map[string]any)["message"] != "Member already exists" {
		t.Fatalf("duplicate = %d %v, want 409 conflict \"Member already exists\"", resp.StatusCode, m)
	}
	if resp, m := postMember(t, s, `{"email":"Alice@x.io","role":"member"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("case variant = %d %v, want 201 (distinct member)", resp.StatusCode, m)
	}
	if got := s.MemberRoles(testOrg); got["alice@x.io"] != "member" || len(got) != 2 {
		t.Fatalf("stored = %v: the refused duplicate must not have changed the role", got)
	}
}

// Fails if PUT is ever allowed, if the 403 depends on the body/role/member
// existing, or if the message drifts.
func TestMemberPutIsAlways403TokenNotPermitted(t *testing.T) {
	s := newClusterFake(t)
	_, m := postMember(t, s, `{"email":"a@x.io","role":"viewer"}`)
	id := m["id"].(string)
	for _, c := range []struct{ name, path, body string }{
		{"valid role", membersPath + "/" + id, `{"role":"admin"}`},
		{"invalid role", membersPath + "/" + id, `{"role":"root"}`},
		{"no body", membersPath + "/" + id, ``},
		{"unknown member", membersPath + "/00000000-0000-4000-8000-000000000000", `{"role":"admin"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, b := call(t, s, http.MethodPut, c.path, c.body)
			mm := decode(t, b)
			if resp.StatusCode != http.StatusForbidden || errCode(mm) != "token_not_permitted" ||
				mm["error"].(map[string]any)["message"] != "API tokens may not modify member roles — use an owner or admin login" {
				t.Fatalf("PUT = %d %s, want 403 token_not_permitted with the measured message", resp.StatusCode, b)
			}
		})
	}
	if got := s.MemberRoles(testOrg)["a@x.io"]; got != "viewer" {
		t.Fatalf("role became %q: a refused PUT must not modify anything", got)
	}
}

// Fails if GET of a missing member is not 404, or if DELETE is not 204 then 404.
func TestMemberGetAndDeleteLifecycle(t *testing.T) {
	s := newClusterFake(t)
	_, m := postMember(t, s, `{"email":"a@x.io","role":"viewer"}`)
	p := membersPath + "/" + m["id"].(string)
	if resp, b := call(t, s, http.MethodGet, p, ""); resp.StatusCode != http.StatusOK || decode(t, b)["email"] != "a@x.io" {
		t.Fatalf("GET = %d %s", resp.StatusCode, b)
	}
	if resp, b := call(t, s, http.MethodDelete, p, ""); resp.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("DELETE = %d %q, want 204 empty", resp.StatusCode, b)
	}
	if resp, _ := call(t, s, http.MethodDelete, p, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodGet, p, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE = %d, want 404", resp.StatusCode)
	}
	// the freed email can be added again
	if resp, _ := postMember(t, s, `{"email":"a@x.io","role":"viewer"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-POST = %d, want 201", resp.StatusCode)
	}
}

// Fails if the fake refuses to delete the LAST owner (the real API has no
// guard; the provider must supply it).
func TestMemberDeleteHasNoLastOwnerGuard(t *testing.T) {
	s := newClusterFake(t)
	_, m := postMember(t, s, `{"email":"o@x.io","role":"owner"}`)
	if resp, _ := call(t, s, http.MethodDelete, membersPath+"/"+m["id"].(string), ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("deleting the only owner = %d, want 204", resp.StatusCode)
	}
}

// Fails if another org's member is visible through this org's path.
func TestMemberOfAnotherOrgIs404(t *testing.T) {
	s := newClusterFake(t)
	const other = "33333333-3333-4333-8333-333333333333"
	s.AddOrg(other, "Other", "other")
	id := s.SeedMember(other, "z@x.io", "viewer")
	if resp, _ := call(t, s, http.MethodGet, membersPath+"/"+id, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-org GET = %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodDelete, membersPath+"/"+id, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-org DELETE = %d, want 404", resp.StatusCode)
	}
}

// Fails if the list ignores limit, omits next_cursor on a full page, keeps
// paging past the end, repeats/skips a member across pages, or does not clamp.
func TestMemberListPaginatesByIDWithNextCursorOnAFullPage(t *testing.T) {
	s := newClusterFake(t)
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		want[s.SeedMember(testOrg, fmt.Sprintf("u%d@x.io", i), "owner")] = true
	}
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		path := membersPath + "?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp, b := call(t, s, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list = %d %s", resp.StatusCode, b)
		}
		m := decode(t, b)
		items := m["items"].([]any)
		if len(items) > 2 {
			t.Fatalf("page has %d items with limit=2", len(items))
		}
		for _, it := range items {
			id := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("member %s repeated across pages", id)
			}
			seen[id] = true
		}
		pages++
		nc, has := m["next_cursor"]
		if len(items) == 2 != has {
			t.Fatalf("page %d: %d items, next_cursor present=%v: must be set exactly when the page is full", pages, len(items), has)
		}
		if !has {
			break
		}
		cursor = nc.(string)
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 5 || pages != 3 {
		t.Fatalf("saw %d members over %d pages, want all 5 over 3 (2+2+1)", len(seen), pages)
	}
	// A page that is exactly full still carries the cursor (the following page is empty).
	_, b := call(t, s, http.MethodGet, membersPath+"?limit=5", "")
	if _, has := decode(t, b)["next_cursor"]; !has {
		t.Fatalf("exactly-full page lacks next_cursor: %s", b)
	}
	// limit clamps: 0 -> 1, 1000 -> 100; junk/absent -> 50
	_, b = call(t, s, http.MethodGet, membersPath+"?limit=0", "")
	if n := len(decode(t, b)["items"].([]any)); n != 1 {
		t.Fatalf("limit=0 returned %d items, want clamp to 1", n)
	}
	_, b = call(t, s, http.MethodGet, membersPath, "")
	if _, has := decode(t, b)["next_cursor"]; has {
		t.Fatalf("5 members under the default limit must not carry next_cursor")
	}
}

// Fails if SeedMemberWithID does not control list order (the resource-level
// pagination test depends on the seeded owner sorting last), or if a seeded
// row is not served by GET.
func TestSeedMemberWithIDControlsListPosition(t *testing.T) {
	s := newClusterFake(t)
	const last = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	s.SeedMemberWithID(testOrg, last, "last@x.io", "owner")
	for i := 0; i < 3; i++ {
		s.SeedMember(testOrg, fmt.Sprintf("u%d@x.io", i), "viewer")
	}
	_, b := call(t, s, http.MethodGet, membersPath+"?limit=3", "")
	m := decode(t, b)
	for _, it := range m["items"].([]any) {
		if it.(map[string]any)["id"] == last {
			t.Fatalf("id %s (max) appeared on the first page of 3 of 4: %s", last, b)
		}
	}
	if resp, gb := call(t, s, http.MethodGet, membersPath+"/"+last, ""); resp.StatusCode != http.StatusOK || decode(t, gb)["role"] != "owner" {
		t.Fatalf("GET seeded = %d %s", resp.StatusCode, gb)
	}
}
