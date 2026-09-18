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
}

type Server struct {
	t     testing.TB
	srv   *httptest.Server
	token string

	mu          sync.Mutex
	orgs        []Org
	requests    []Request
	bodyReadErr error
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
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "invalid token"}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
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
	s.requests = append(s.requests, Request{Method: r.Method, Path: path, Body: body})
	h := s.Handlers[r.Method+" "+path]
	s.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/organizations/") && strings.Count(path, "/") == 2:
		id := strings.TrimPrefix(path, "/organizations/")
		s.mu.Lock()
		var found *Org
		for i := range s.orgs {
			if s.orgs[i].ID == id {
				found = &s.orgs[i]
			}
		}
		s.mu.Unlock()
		if found == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "organization not found"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": found.ID, "name": found.Name, "slug": found.Slug, "plan": "free",
			"created_at": "2026-09-17T00:00:00Z", "updated_at": "2026-09-17T00:00:00Z"})
	case r.Method == http.MethodGet && path == "/organizations":
		s.mu.Lock()
		items := make([]map[string]any, 0, len(s.orgs))
		for _, o := range s.orgs {
			items = append(items, map[string]any{"id": o.ID, "name": o.Name, "slug": o.Slug, "plan": "free",
				"created_at": "2026-09-17T00:00:00Z", "updated_at": "2026-09-17T00:00:00Z"})
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "no route " + r.Method + " " + path}})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteJSON is exported for test handlers.
func WriteJSON(w http.ResponseWriter, code int, v any) { writeJSON(w, code, v) }
