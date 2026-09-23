package fakeapi

// Member routes of the fake, reproducing what aether-api DOES (measured at
// aether-api 8e9045a; SDD task-9-measured-contract.md §7-§10):
//
//	GET    /organizations/{org}/members         200 {items[], next_cursor?}
//	POST   /organizations/{org}/members         201 Member | 400 | 409 | 422
//	GET    /organizations/{org}/members/{mid}   200 Member | 404
//	PUT    /organizations/{org}/members/{mid}   403 token_not_permitted, ALWAYS
//	DELETE /organizations/{org}/members/{mid}   204 | 404
//
// Behaviours that matter and that the fake copies:
//   - POST creates an immediately effective MEMBERSHIP row (no invitation, no
//     Keycloak account). The body is exactly {email, role}, both required; an
//     unknown key is 400. 422 details: email/required/"Email is required",
//     role/required/"Role is required", role/invalid/"Role must be one of:
//     owner, admin, member, viewer". The email is stored VERBATIM and
//     uniqueness is case-sensitive: an exact duplicate is 409 conflict
//     "Member already exists", "Alice@x" and "alice@x" are two members. No
//     role gate: any token may add any role, owner included;
//   - the response Member carries id (the membership row id), org_id, email,
//     role, created_at and NEVER invited_by / accepted_at (no code path writes
//     them on API-created rows);
//   - PUT is 403 token_not_permitted "API tokens may not modify member roles
//     - use an owner or admin login" for EVERY token, decided before the body
//     is read or the member looked up (§8). The API's 500 for an invalid role
//     on PUT is therefore unreachable with a token and is not modelled;
//   - GET/DELETE of a missing member, or another org's member, is 404. DELETE
//     answers 204 and a second DELETE 404. There is no last-owner or
//     self-removal guard (that is the provider's job);
//   - the list is ordered by id, "id > cursor", limit clamped to 1..100
//     (default 50), and next_cursor (the last id of the page) is emitted
//     whenever the page is FULL - even when nothing follows it (§10).

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
)

var (
	membersPathRE = regexp.MustCompile(`^/organizations/([^/]+)/members$`)
	memberPathRE  = regexp.MustCompile(`^/organizations/([^/]+)/members/([^/]+)$`)
)

var memberRoles = map[string]bool{"owner": true, "admin": true, "member": true, "viewer": true}

type fakeMember struct {
	id, email, role string
	created         int // creation order, for a monotonic created_at
}

// memberStore is guarded by the enclosing orgStore.mu.
type memberStore struct {
	byOrg map[string][]*fakeMember
	seq   int
}

func (ms *memberStore) add(org, email, role string) *fakeMember {
	if ms.byOrg == nil {
		ms.byOrg = map[string][]*fakeMember{}
	}
	ms.seq++
	m := &fakeMember{id: uuid.NewString(), email: email, role: role, created: ms.seq}
	ms.byOrg[org] = append(ms.byOrg[org], m)
	return m
}

func (ms *memberStore) find(org, id string) (*fakeMember, int) {
	for i, m := range ms.byOrg[org] {
		if m.id == id {
			return m, i
		}
	}
	return nil, -1
}

func memberJSON(org string, m *fakeMember) map[string]any {
	return map[string]any{
		"id": m.id, "org_id": org, "email": m.email, "role": m.role,
		"created_at": orgEpoch.Add(time.Duration(m.created) * time.Second).Format(time.RFC3339),
	}
}

// SeedMember adds a membership out of band (no request is recorded) and
// returns its id. It applies no validation and no uniqueness check.
func (s *Server) SeedMember(org, email, role string) string {
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	return s.orgStore.members.add(org, email, role).id
}

// SeedMemberWithID is SeedMember with a caller-chosen membership id, so a test
// can decide where the row sorts in the id-ordered list (the pagination test
// puts the only "other owner" on the LAST page).
func (s *Server) SeedMemberWithID(org, id, email, role string) {
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	s.orgStore.members.add(org, email, role).id = id
}

// RemoveMember deletes a membership out of band (no request is recorded).
func (s *Server) RemoveMember(org, id string) {
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	if _, i := s.orgStore.members.find(org, id); i >= 0 {
		l := s.orgStore.members.byOrg[org]
		s.orgStore.members.byOrg[org] = append(l[:i:i], l[i+1:]...)
	}
}

// MemberRoles returns email -> role for the org, for assertions on what the
// fake really stored.
func (s *Server) MemberRoles(org string) map[string]string {
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	out := map[string]string{}
	for _, m := range s.orgStore.members.byOrg[org] {
		out[m.email] = m.role
	}
	return out
}

// routeMembers serves the member routes; it reports false when path is not one.
func (s *Server) routeMembers(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	var org, mid string
	list := false
	if m := membersPathRE.FindStringSubmatch(path); m != nil {
		org, list = m[1], true
	} else if m := memberPathRE.FindStringSubmatch(path); m != nil {
		org, mid = m[1], m[2]
	} else {
		return false
	}
	if !s.orgAccess(w, org) {
		return true
	}
	if list {
		switch r.Method {
		case http.MethodGet:
			s.listMembers(w, r, org)
		case http.MethodPost:
			s.createMember(w, org, body)
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
		return true
	}
	switch r.Method {
	case http.MethodGet:
		s.orgStore.mu.Lock()
		m, _ := s.orgStore.members.find(org, mid)
		s.orgStore.mu.Unlock()
		if m == nil {
			apiError(w, http.StatusNotFound, "not_found", "Member not found")
			return true
		}
		writeJSON(w, http.StatusOK, memberJSON(org, m))
	case http.MethodPut:
		// Before the body and before any lookup: a nonexistent member id gets
		// the same 403 (§8).
		apiError(w, http.StatusForbidden, "token_not_permitted", "API tokens may not modify member roles — use an owner or admin login")
	case http.MethodDelete:
		s.orgStore.mu.Lock()
		m, _ := s.orgStore.members.find(org, mid)
		s.orgStore.mu.Unlock()
		if m == nil {
			apiError(w, http.StatusNotFound, "not_found", "Member not found")
			return true
		}
		s.RemoveMember(org, mid)
		w.WriteHeader(http.StatusNoContent)
	default:
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
	}
	return true
}

func (s *Server) createMember(w http.ResponseWriter, org string, body []byte) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeStrict(body, &req) {
		apiError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	var details []map[string]any
	if req.Email == "" {
		details = append(details, detail("email", "required", "Email is required"))
	}
	switch {
	case req.Role == "":
		details = append(details, detail("role", "required", "Role is required"))
	case !memberRoles[req.Role]:
		details = append(details, detail("role", "invalid", "Role must be one of: owner, admin, member, viewer"))
	}
	if details != nil {
		validationError(w, details)
		return
	}
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	for _, m := range s.orgStore.members.byOrg[org] {
		if m.email == req.Email { // exact, case-sensitive
			apiError(w, http.StatusConflict, "conflict", "Member already exists")
			return
		}
	}
	m := s.orgStore.members.add(org, req.Email, req.Role)
	writeJSON(w, http.StatusCreated, memberJSON(org, m))
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request, org string) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = min(max(n, 1), 100)
		}
	}
	cursor := r.URL.Query().Get("cursor")
	s.orgStore.mu.Lock()
	all := append([]*fakeMember(nil), s.orgStore.members.byOrg[org]...)
	s.orgStore.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	items := []map[string]any{}
	var last string
	for _, m := range all {
		if cursor != "" && m.id <= cursor {
			continue
		}
		if len(items) == limit {
			break
		}
		items = append(items, memberJSON(org, m))
		last = m.id
	}
	out := map[string]any{"items": items}
	if len(items) == limit {
		out["next_cursor"] = last
	}
	writeJSON(w, http.StatusOK, out)
}
