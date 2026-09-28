package fakeapi

// Public routes of the fake, reproducing what aether-api DOES (measured at
// aether-api 8e9045a; SDD task-13-measured-contract.md §1-§2):
//
//	GET /regions        200 {"items":[{id,name,location,available}]}  PUBLIC
//	GET /k8s-versions    200 {"items":[{version,label,patch}]}         PUBLIC
//
// Both routes skip the fake's normal bearer-token check entirely
// (isPublicRoute, called from serve() in fakeapi.go BEFORE the auth gate) -
// a request with no Authorization header at all must still succeed here,
// matching the real API's `security: []` (OA:157, OA:252).
//
// Regions: exactly one region on every instance - eu-fi / "Europe (Finland)" /
// "Helsinki" - always available=true. This fake models a single
// eu-fi-serving instance; there is no AETHER_REGION knob because nothing in
// this provider exercises a second region.
//
// K8s-versions: newest first, "Latest" on the FIRST item only, every other
// item "Stable". The key is "items", never "versions". An "unavailable
// window" (cold start, upstream down, nothing runnable) is ALWAYS a 200
// {"items":[]} and never a 503 (contract §2) - reproduced here by calling
// SetK8sVersions with no arguments.

import "net/http"

// K8sVersion is one item of the fake's /k8s-versions list.
type K8sVersion struct {
	Version, Label, Patch string
}

// k8sVersionsStore is embedded by value in Server; its zero value already
// serves the default seeded list (see defaultK8sVersions) - initialized
// distinguishes "SetK8sVersions never called" from "called with zero items",
// which must serve the empty-window 200 rather than the default table.
type k8sVersionsStore struct {
	items       []K8sVersion
	initialized bool
}

// defaultK8sVersions is what a fresh Server serves before any SetK8sVersions
// call: three minors, newest first, with the same patches clusterStore's own
// default table resolves bare minors to (1.37/1.36/1.35) - so a test that
// never calls SetK8sVersions sees numbers consistent with cluster creation
// elsewhere in the same fake.
func defaultK8sVersions() []K8sVersion {
	return []K8sVersion{
		{Version: "1.37", Label: "Latest", Patch: "1.37.2"},
		{Version: "1.36", Label: "Stable", Patch: "1.36.4"},
		{Version: "1.35", Label: "Stable", Patch: "1.35.6"},
	}
}

// SetK8sVersions replaces the /k8s-versions list, in the order given (the
// route never re-sorts: the FIRST item is what reads "Latest"). Called with
// no arguments it makes the list EMPTY - the fake's knob for "the platform's
// version window is unavailable right now" (still 200, never 503, §2).
func (s *Server) SetK8sVersions(items ...K8sVersion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.k8sVersions.items = append([]K8sVersion(nil), items...)
	s.k8sVersions.initialized = true
}

func (s *Server) k8sVersionsList() []K8sVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.k8sVersions.initialized {
		return defaultK8sVersions()
	}
	return append([]K8sVersion(nil), s.k8sVersions.items...)
}

// isPublicRoute reports whether method+path may be served with no bearer
// token at all (contract §1, §2: security: []). Checked BEFORE the auth gate
// in serve().
func isPublicRoute(method, path string) bool {
	return method == http.MethodGet && (path == "/regions" || path == "/k8s-versions")
}

// SetRegionsEmpty forces GET /regions to answer 200 {"items":[]} instead of
// the normal single eu-fi region. Review finding m1: an empty result must
// round-trip through the provider as an EMPTY list, never a null one - this
// is the fake's knob to exercise that (the contract says the list always
// holds one region, so the real API never actually produces this; it is
// reachable only through this knob).
func (s *Server) SetRegionsEmpty(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regionsEmpty = on
}

// routeRegions serves GET /regions; it reports false when path is not that
// route.
func (s *Server) routeRegions(w http.ResponseWriter, r *http.Request, path string, _ []byte) bool {
	if path != "/regions" {
		return false
	}
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		return true
	}
	s.mu.Lock()
	empty := s.regionsEmpty
	s.mu.Unlock()
	if empty {
		writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{}})
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{
		{"id": "eu-fi", "name": "Europe (Finland)", "location": "Helsinki", "available": true},
	}})
	return true
}

// routeK8sVersions serves GET /k8s-versions; it reports false when path is
// not that route.
func (s *Server) routeK8sVersions(w http.ResponseWriter, r *http.Request, path string, _ []byte) bool {
	if path != "/k8s-versions" {
		return false
	}
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		return true
	}
	items := []map[string]any{}
	for _, v := range s.k8sVersionsList() {
		items = append(items, map[string]any{"version": v.Version, "label": v.Label, "patch": v.Patch})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return true
}
