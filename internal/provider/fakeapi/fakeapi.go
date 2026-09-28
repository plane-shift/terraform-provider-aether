// Package fakeapi is an in-process fake of the Aether API for unit tests.
// It speaks exactly the JSON shapes of api/openapi.yaml and records requests.
//
// IDs are UUIDs: every id-shaped field in the generated client
// (internal/client) is typed openapi_types.UUID (github.com/oapi-codegen/runtime/types,
// itself google/uuid.UUID). AddOrg's id argument must therefore be canonical
// UUID text (e.g. "11111111-1111-4111-8111-111111111111") — the fake serves
// it back verbatim as the org's "id", and a caller passing an opaque string
// like "org-1" will fail to decode on the client side, not inside this fake.
package fakeapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type Org struct {
	ID, Name, Slug string
}

type Request struct {
	Method, Path string
	Body         []byte
	// Query is the request's raw query string (no leading "?", "" when there
	// is none). Routing itself never looks at this field - only r.URL.Path
	// (CutPrefix above) - so it exists purely for a test to assert on a
	// PROVIDER-sent query parameter (registry robot-account paging's
	// `?cursor=`, the registry disable's `?purge=`) that the recorded Path
	// alone cannot show.
	Query string
}

type Server struct {
	t     testing.TB
	srv   *httptest.Server
	token string

	mu           sync.Mutex
	orgs         []Org
	requests     []Request
	bodyReadErr  error
	clusters     clusterStore     // see clusters.go; own lock
	pools        poolStore        // see nodepools.go; own lock
	networks     networkStore     // see networks.go; own lock
	orgStore     orgStore         // see organizations.go + members.go; own lock
	webhooks     webhookStore     // see webhooks.go; own lock
	registries   registryStore    // see registry.go; own lock
	budgets      budgetStore      // see budgets.go; own lock
	k8sVersions  k8sVersionsStore // see regions.go; own lock
	regionsEmpty bool             // see regions.go SetRegionsEmpty; protected by mu
	kubeconfigs  kubeconfigStore  // see kubeconfig.go; own lock
	lbs          lbStore          // see loadbalancers.go; own lock
	volumes      volumeStore      // see volumes.go; own lock
	machines     machineStore     // see machines.go; own lock
	// Handlers lets a test override a route: key "METHOD /path" (path after /api/v1).
	Handlers map[string]http.HandlerFunc
}

func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, token: "aether_" + strings.Repeat("a", 64), Handlers: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) URL() string   { return s.srv.URL }
func (s *Server) Token() string { return s.token }

// AddOrg registers an organization. id must be canonical UUID text — see the
// package doc comment.
func (s *Server) AddOrg(id, name, slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orgs = append(s.orgs, Org{ID: id, Name: name, Slug: slug})
}

// BodyReadError returns the last request-body read error the fake
// encountered (e.g. a client that promised more bytes via Content-Length
// than it sent), or nil. A test that deliberately breaks a body mid-transfer
// should assert on this instead of expecting a panic/Fatalf from within the
// handler goroutine, which cannot safely fail the test that created the
// server.
func (s *Server) BodyReadError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodyReadErr
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, "/api/v1")
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "invalid API base path " + r.URL.Path}})
		return
	}
	// /regions and /k8s-versions carry NO auth check at all (measured contract
	// §1/§2, security: []) - a request with no Authorization header must still
	// succeed, so the path must be known before the token gate runs.
	if !isPublicRoute(r.Method, path) && r.Header.Get("Authorization") != "Bearer "+s.token {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "invalid token"}})
		return
	}
	var body []byte
	if r.Body != nil {
		// io.ReadAll, not a single Read call: httptest bodies can arrive
		// chunked, and a single Read is not guaranteed to drain them —
		// truncating the recorded body out from under a test that inspects it.
		b, err := io.ReadAll(r.Body)
		if err != nil {
			// serve runs on a per-connection goroutine net/http spawns, NOT
			// the goroutine that called New(t) and owns t. t.Fatalf here
			// would call runtime.Goexit on the WRONG goroutine: it ends
			// quietly without ever writing a response, and the client sees a
			// bare connection close/EOF that names nothing - the test that
			// triggered this then fails on an unrelated-looking error
			// instead of on the actual cause. Record it and answer instead.
			s.mu.Lock()
			s.bodyReadErr = err
			s.mu.Unlock()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal", "message": "fakeapi: reading request body: " + err.Error()}})
			return
		}
		body = b
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: path, Body: body, Query: r.URL.RawQuery})
	h := s.Handlers[r.Method+" "+path]
	s.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	switch {
	case s.routeRegions(w, r, path, body):
		// served by regions.go
	case s.routeK8sVersions(w, r, path, body):
		// served by regions.go
	case s.routeOrganizations(w, r, path, body):
		// served by organizations.go
	case s.routeMembers(w, r, path, body):
		// served by members.go
	case r.Method == http.MethodGet && path == "/organizations":
		s.mu.Lock()
		orgs := append([]Org(nil), s.orgs...)
		s.mu.Unlock()
		items := make([]map[string]any, 0, len(orgs))
		for _, o := range orgs {
			s.orgStore.mu.Lock()
			foreign := s.orgStore.foreign[o.ID]
			s.orgStore.mu.Unlock()
			if !foreign { // the token lists only the organizations it reaches
				items = append(items, s.orgJSON(o))
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case s.routeNodePools(w, r, path, body):
		// served by nodepools.go
	case s.routeMachines(w, r, path, body):
		// served by machines.go
	case s.routeClusterNetwork(w, r, path, body):
		// served by networks.go
	case s.routeKubeconfig(w, r, path, body):
		// served by kubeconfig.go
	case s.routeLoadBalancers(w, r, path, body):
		// served by loadbalancers.go
	case s.routeVolumes(w, r, path, body):
		// served by volumes.go
	case s.routeClusters(w, r, path, body):
		// served by clusters.go
	case s.routeWebhooks(w, r, path, body):
		// served by webhooks.go
	case s.routeRegistry(w, r, path, body):
		// served by registry.go
	case s.routeBudget(w, r, path, body):
		// served by budgets.go
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "no route " + r.Method + " " + path}})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// errB is marshalling a plain map of strings, which cannot itself
		// fail - safe to ignore.
		errB, _ := json.Marshal(map[string]any{"error": map[string]any{"code": "internal", "message": "fakeapi: marshalling response: " + err.Error()}})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(errB)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(b); err != nil {
		// serve runs on a per-connection goroutine, not the test's own
		// goroutine (see the read-error path above) - a write failure here
		// just means the client already went away; nothing more to do.
		return
	}
}

// WriteJSON is exported for test handlers.
func WriteJSON(w http.ResponseWriter, code int, v any) { writeJSON(w, code, v) }
