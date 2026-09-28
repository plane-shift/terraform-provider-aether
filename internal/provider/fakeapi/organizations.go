package fakeapi

// Organization routes of the fake, reproducing what aether-api DOES (measured
// at aether-api 8e9045a; SDD task-9-measured-contract.md), not what its
// OpenAPI spec says:
//
//	GET    /organizations/{id}   200 Organization | 403 | 404
//	PUT    /organizations/{id}   200 Organization | 400 | 403 | 404 | 409 | 422
//	DELETE /organizations/{id}   403 token_not_permitted (every token, §5)
//
// (GET /organizations, the list Configure resolves the org from, stays in
// fakeapi.go and renders through the same orgJSON.)
//
// Behaviours a Terraform provider must survive, and the fake copies:
//   - PUT is a PARTIAL MERGE (SQL COALESCE): an omitted key or a JSON null
//     KEEPS the stored value; the empty string is the ONLY clear, is stored
//     as "" and reads back as "" (never absent again). NULL cannot be
//     written back (§3);
//   - billing_email / vat_id are ABSENT from the body while never set, and
//     present (possibly "") once written (§2);
//   - name, slug, billing_email and vat_id are NOT validated on PUT: any
//     string, "" included (§3). Only a duplicate slug is refused: 409
//     conflict "Organization already exists";
//   - updated_at moves on EVERY successful PUT, including `{}` (DB trigger);
//   - an unknown JSON key is a 400 bad_request; the seven billing-detail keys
//     the vendored client cannot send are accepted and ignored;
//   - plan is always present ("starter") and never writable;
//   - there is NO role gate on PUT;
//   - an org the token does not reach (AddForeignOrg) answers 403 forbidden
//     "You do not have access to this organization" on every route, never 404
//     (§11). A wholly UNREGISTERED id stays 404, which existing Configure
//     tests depend on.

import (
	"net/http"
	"regexp"
	"sync"
	"time"
)

var orgPathRE = regexp.MustCompile(`^/organizations/([^/]+)$`)

// fakeOrgExtras is what the API stores beyond the name/slug kept in
// Server.orgs. nil = SQL NULL = key absent from the response.
type fakeOrgExtras struct {
	billingEmail *string
	vatID        *string
	mutations    int // every PUT increments it, whatever it carried
}

// orgStore is embedded by value in Server; its zero value is usable.
type orgStore struct {
	mu      sync.Mutex
	extras  map[string]*fakeOrgExtras
	foreign map[string]bool
	members memberStore
}

// orgEpoch anchors updated_at: only relative movement matters to a caller.
var orgEpoch = time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)

const orgCreatedAt = "2026-09-17T00:00:00Z"

// AddForeignOrg registers an organization that exists on the platform but that
// the fake's token cannot reach: every org-scoped route answers 403 for it.
func (s *Server) AddForeignOrg(id, name, slug string) {
	s.mu.Lock()
	s.orgs = append(s.orgs, Org{ID: id, Name: name, Slug: slug})
	s.mu.Unlock()
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	if s.orgStore.foreign == nil {
		s.orgStore.foreign = map[string]bool{}
	}
	s.orgStore.foreign[id] = true
}

// SetOrgBilling seeds billing_email / vat_id out of band. nil = NULL (absent).
func (s *Server) SetOrgBilling(id string, billingEmail, vatID *string) {
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	x := s.orgStore.extrasFor(id)
	x.billingEmail, x.vatID = billingEmail, vatID
}

// extrasFor returns (creating) the extras row of id. Caller holds os.mu.
func (os *orgStore) extrasFor(id string) *fakeOrgExtras {
	if os.extras == nil {
		os.extras = map[string]*fakeOrgExtras{}
	}
	x := os.extras[id]
	if x == nil {
		x = &fakeOrgExtras{}
		os.extras[id] = x
	}
	return x
}

// orgJSON renders the Organization schema for o. It takes the extras lock
// itself, so callers must NOT hold orgStore.mu.
func (s *Server) orgJSON(o Org) map[string]any {
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	return s.orgJSONLocked(o)
}

func (s *Server) orgJSONLocked(o Org) map[string]any {
	x := s.orgStore.extrasFor(o.ID)
	m := map[string]any{
		"id": o.ID, "name": o.Name, "slug": o.Slug, "plan": "starter",
		"created_at": orgCreatedAt,
		"updated_at": orgEpoch.Add(time.Duration(x.mutations) * time.Second).Format(time.RFC3339),
	}
	if x.billingEmail != nil {
		m["billing_email"] = *x.billingEmail
	}
	if x.vatID != nil {
		m["vat_id"] = *x.vatID
	}
	return m
}

// orgAccess resolves an org id the way every org-scoped route must: an
// unregistered id is 404, a foreign one 403. It writes the error and returns
// false when the caller must stop.
func (s *Server) orgAccess(w http.ResponseWriter, id string) bool {
	if !s.orgKnown(id) {
		apiError(w, http.StatusNotFound, "not_found", "organization not found")
		return false
	}
	s.orgStore.mu.Lock()
	foreign := s.orgStore.foreign[id]
	s.orgStore.mu.Unlock()
	if foreign {
		apiError(w, http.StatusForbidden, "forbidden", "You do not have access to this organization")
		return false
	}
	return true
}

// routeOrganizations serves /organizations/{id}; it reports false when path
// is not that route. Member routes are in members.go.
func (s *Server) routeOrganizations(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	m := orgPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	id := m[1]
	if !s.orgAccess(w, id) {
		return true
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		o := s.orgByID(id)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, s.orgJSON(o))
	case http.MethodPut:
		s.putOrg(w, id, body)
	case http.MethodDelete:
		apiError(w, http.StatusForbidden, "token_not_permitted", "API tokens may not delete organizations")
	default:
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
	}
	return true
}

// orgByID returns the org row. Caller holds s.mu and has checked existence.
func (s *Server) orgByID(id string) Org {
	for _, o := range s.orgs {
		if o.ID == id {
			return o
		}
	}
	return Org{}
}

// putOrg is the COALESCE merge described in the file comment.
func (s *Server) putOrg(w http.ResponseWriter, id string, body []byte) {
	var req struct {
		Name         *string `json:"name"`
		Slug         *string `json:"slug"`
		BillingEmail *string `json:"billing_email"`
		VatID        *string `json:"vat_id"`
		// Accepted by the real API, unreachable through the vendored client:
		// present only so they are not mistaken for unknown keys.
		CustomerType   any `json:"customer_type"`
		LegalName      any `json:"legal_name"`
		FirstName      any `json:"first_name"`
		LastName       any `json:"last_name"`
		BillingAddress any `json:"billing_address"`
		PrimaryContact any `json:"primary_contact"`
		CountryCode    any `json:"country_code"`
	}
	if !decodeStrict(body, &req) {
		apiError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	s.orgStore.mu.Lock()
	defer s.orgStore.mu.Unlock()
	s.mu.Lock()
	if req.Slug != nil {
		for _, o := range s.orgs {
			if o.ID != id && o.Slug == *req.Slug {
				s.mu.Unlock()
				apiError(w, http.StatusConflict, "conflict", "Organization already exists")
				return
			}
		}
	}
	var cur Org
	for i := range s.orgs {
		if s.orgs[i].ID != id {
			continue
		}
		// COALESCE(nil, stored): only a non-nil pointer overwrites, and "" is
		// a value like any other.
		if req.Name != nil {
			s.orgs[i].Name = *req.Name
		}
		if req.Slug != nil {
			s.orgs[i].Slug = *req.Slug
		}
		cur = s.orgs[i]
	}
	s.mu.Unlock()
	x := s.orgStore.extrasFor(id)
	if req.BillingEmail != nil {
		v := *req.BillingEmail
		x.billingEmail = &v
	}
	if req.VatID != nil {
		v := *req.VatID
		x.vatID = &v
	}
	x.mutations++ // the BEFORE UPDATE trigger fires on every PUT, `{}` included
	writeJSON(w, http.StatusOK, s.orgJSONLocked(cur))
}
