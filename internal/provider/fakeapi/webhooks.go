package fakeapi

// Webhook routes of the fake, reproducing what aether-api DOES (measured at
// aether-api 8e9045a; SDD task-10-measured-contract.md), not what its OpenAPI
// spec says. `/test` and `/deliveries` are OUT OF SCOPE (contract §8) and are
// not served here - a request to either falls through to the fake's normal
// 404 "no route".
//
//	GET    /organizations/{org}/webhooks                      200 {items[], next_cursor?}
//	POST   /organizations/{org}/webhooks                      201 Webhook (WITH secret) | 400 | 409 | 422
//	GET    /organizations/{org}/webhooks/{id}                 200 Webhook (no secret) | 404
//	PUT    /organizations/{org}/webhooks/{id}                 200 Webhook (no secret) | 400 | 404 | 422
//	POST   /organizations/{org}/webhooks/{id}/rotate-secret    200 Webhook (WITH a NEW secret) | 404
//	DELETE /organizations/{org}/webhooks/{id}                 204 | 404
//
// Behaviours that matter and that the fake copies (contract section numbers
// below):
//   - POST's body is exactly {url, description?, events}; `enabled` or
//     `secret` in the body is a 400 bad_request "Invalid JSON body"
//     (DisallowUnknownFields) - a webhook can only be CREATED enabled (§2).
//     description defaults to "" when omitted, never null;
//   - validation stops at the first failing step (§2): url=="" AND
//     events empty/absent give BOTH detail entries at once
//     ({url,required,"URL is required"} and/or
//     {events,required,"At least one event is required"}); only once both
//     pass does event-catalog validation run (unknown type, or "*" not
//     alone), then URL validation (parse error, non-https scheme,
//     credentials, a literal blocked-range IP - hostnames are NOT resolved,
//     so https://localhost/ passes, §2/I3);
//   - events are stored EXACTLY as sent: no de-duplication, no sorting, no
//     wildcard normalisation (§2/§3/I4);
//   - the 11th webhook of an org - disabled ones included - is 409
//     webhook_limit "Webhook limit reached for this organization" (§2);
//   - GET, PUT and the list response NEVER include `secret` (it is not
//     selected by the real store); only the 201 create response and the 200
//     rotate response carry it (§2/§4/§6, WRONG #1 in the contract's "wrong
//     in the plan" section);
//   - PUT is a partial merge (SQL COALESCE): omitted/null keeps a field,
//     `description:""` clears it, `events:[]` is 422
//     "at least one event type is required" (a DIFFERENT message from
//     create's), a present url/events is re-validated, `enabled` is a plain
//     pointer merge, and `{}` is valid and still bumps updated_at (§5);
//   - rotate-secret takes no body, is allowed on a disabled webhook, always
//     issues a brand new secret (never idempotent) and bumps updated_at (§6);
//   - DELETE is a hard delete: 204 then a second DELETE (or a GET) is 404,
//     never 410 (§7);
//   - an unknown JSON key on POST or PUT, `secret` included, is 400 (§1/§5);
//   - a webhook id belonging to another org, under THIS org's path, is a
//     404; the org itself being foreign (AddForeignOrg) is a 403, handled by
//     the shared orgAccess helper before any webhook-specific code runs
//     (§4/§9);
//   - FailNextWebhookPUT / FailNextWebhookRotate let a test force the FIRST
//     PUT or rotate call after they are armed to fail with a chosen status,
//     consumed after one use. They exist because a create-then-PUT (a
//     disabled-at-create webhook, T10-3) or an update-then-rotate (T10-8)
//     targets a webhook whose id is not known until the FIRST call of the
//     pair completes - unlike every other per-id knob in this fake, the
//     override cannot be keyed by id.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	webhooksPathRE      = regexp.MustCompile(`^/organizations/([^/]+)/webhooks$`)
	webhookPathRE       = regexp.MustCompile(`^/organizations/([^/]+)/webhooks/([^/]+)$`)
	webhookRotatePathRE = regexp.MustCompile(`^/organizations/([^/]+)/webhooks/([^/]+)/rotate-secret$`)
)

// webhookCap is the per-organization limit (contract §2): the 11th webhook,
// disabled ones counted, is 409 webhook_limit.
const webhookCap = 10

// webhookCatalog is the exactly-9 subscribable event catalog (contract §3).
// "*" is accepted too, but only as the sole entry - handled separately in
// validateWebhookEvents, not folded into this map.
var webhookCatalog = map[string]bool{
	"cluster.created":   true,
	"cluster.ready":     true,
	"cluster.upgraded":  true,
	"cluster.degraded":  true,
	"cluster.deleted":   true,
	"node.replaced":     true,
	"usage.alert":       true,
	"invoice.generated": true,
	"ticket.updated":    true,
}

type fakeWebhook struct {
	id, orgID, url, description string
	events                      []string
	enabled                     bool
	secret                      string
	createdSeq, updatedSeq      int // seconds since webhookEpoch; createdSeq never changes
}

// webhookFailure is one armed one-shot override; see FailNextWebhookPUT.
type webhookFailure struct {
	status        int
	code, message string
}

// webhookStore is embedded by value in Server; its zero value is usable.
type webhookStore struct {
	mu    sync.Mutex
	byOrg map[string][]*fakeWebhook
	seq   int // monotonic: created_at/updated_at both derive from it, so every mutation strictly advances the clock

	// One-shot overrides, keyed by org; see FailNextWebhookPUT/Rotate.
	failNextPUT    map[string]*webhookFailure
	failNextRotate map[string]*webhookFailure
}

func (ws *webhookStore) init() {
	if ws.byOrg == nil {
		ws.byOrg = map[string][]*fakeWebhook{}
	}
}

func (ws *webhookStore) find(org, id string) (*fakeWebhook, int) {
	for i, wh := range ws.byOrg[org] {
		if wh.id == id {
			return wh, i
		}
	}
	return nil, -1
}

// webhookEpoch anchors created_at/updated_at: only relative movement and
// created_at == updated_at at creation matter to a caller, never this value.
var webhookEpoch = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

func webhookTime(seq int) string {
	return webhookEpoch.Add(time.Duration(seq) * time.Second).Format(time.RFC3339)
}

func newWebhookSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b) // crypto/rand.Read never errors on the returned io.Reader
	return "whsec_" + hex.EncodeToString(b)
}

// webhookJSON renders the Webhook schema. secret is included only for the
// create (201) and rotate (200) responses - never GET, PUT or list (§2/§4/§6).
func webhookJSON(orgID string, wh *fakeWebhook, includeSecret bool) map[string]any {
	m := map[string]any{
		"id": wh.id, "org_id": orgID, "url": wh.url, "description": wh.description,
		"events": append([]string{}, wh.events...), "enabled": wh.enabled,
		"created_at": webhookTime(wh.createdSeq),
		"updated_at": webhookTime(wh.updatedSeq),
	}
	if includeSecret {
		m["secret"] = wh.secret
	}
	return m
}

// SeedWebhook adds a webhook out of band (no request is recorded, no
// validation applied - it can store what the real API would refuse, such as
// duplicate events) and returns its id.
func (s *Server) SeedWebhook(org, rawURL string, events []string, enabled bool) string {
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.init()
	ws.seq++
	wh := &fakeWebhook{
		id: uuid.NewString(), orgID: org, url: rawURL,
		events: append([]string(nil), events...), enabled: enabled, secret: newWebhookSecret(),
		createdSeq: ws.seq, updatedSeq: ws.seq,
	}
	ws.byOrg[org] = append(ws.byOrg[org], wh)
	return wh.id
}

// RemoveWebhook deletes a webhook out of band (no request is recorded), as a
// portal deletion or a webhook's own dead-lettering never does but a test
// simulating "removed elsewhere" needs.
func (s *Server) RemoveWebhook(org, id string) {
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if _, i := ws.find(org, id); i >= 0 {
		l := ws.byOrg[org]
		ws.byOrg[org] = append(l[:i:i], l[i+1:]...)
	}
}

// WebhookIDs returns the current webhook ids of org, in creation order - for
// a test that needs to see what the fake holds WITHOUT going through
// Terraform state (e.g. proving a failed operation neither deleted nor
// duplicated the underlying object, or that a later replace's DELETE
// targeted the id an earlier failed Create had already saved).
func (s *Server) WebhookIDs(org string) []string {
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	out := make([]string, 0, len(ws.byOrg[org]))
	for _, wh := range ws.byOrg[org] {
		out = append(out, wh.id)
	}
	return out
}

// SetWebhookEvents overwrites a webhook's stored events out of band (no
// request recorded, no validation, no updated_at bump) - a portal write or a
// direct DB edit could store duplicates the create/update validators refuse
// (§2/§3, U2). It reports whether the webhook exists.
func (s *Server) SetWebhookEvents(org, id string, events []string) bool {
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	wh, _ := ws.find(org, id)
	if wh == nil {
		return false
	}
	wh.events = append([]string(nil), events...)
	return true
}

// FailNextWebhookPUT makes the NEXT PUT to any webhook of org answer with
// (status, code, message) instead of being applied to the store, consumed
// after one use. It exists because a create-then-PUT (a webhook created
// disabled, T10-3) targets a webhook whose id is not known until the POST
// completes, so - unlike this fake's other per-id knobs - the override
// cannot be keyed by id.
func (s *Server) FailNextWebhookPUT(org string, status int, code, message string) {
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.failNextPUT == nil {
		ws.failNextPUT = map[string]*webhookFailure{}
	}
	ws.failNextPUT[org] = &webhookFailure{status: status, code: code, message: message}
}

// FailNextWebhookRotate is FailNextWebhookPUT's rotate-secret sibling (T10-8:
// an update-then-rotate whose rotate targets an id not known beforehand).
func (s *Server) FailNextWebhookRotate(org string, status int, code, message string) {
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.failNextRotate == nil {
		ws.failNextRotate = map[string]*webhookFailure{}
	}
	ws.failNextRotate[org] = &webhookFailure{status: status, code: code, message: message}
}

// consumeFailure pops (and clears) an armed override for org, or reports none
// armed. Reading from a nil map is safe in Go, so no init is needed here.
func (ws *webhookStore) consumeFailure(m map[string]*webhookFailure, org string) *webhookFailure {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	f, ok := m[org]
	if !ok {
		return nil
	}
	delete(m, org)
	return f
}

// routeWebhooks serves the webhook routes; it reports false when path is not
// one. /test and /deliveries are out of scope and fall through to the
// caller's default 404.
func (s *Server) routeWebhooks(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	if m := webhookRotatePathRE.FindStringSubmatch(path); m != nil {
		org, id := m[1], m[2]
		if !s.orgAccess(w, org) {
			return true
		}
		if r.Method != http.MethodPost {
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
			return true
		}
		s.rotateWebhookSecret(w, org, id)
		return true
	}
	if m := webhookPathRE.FindStringSubmatch(path); m != nil {
		org, id := m[1], m[2]
		if !s.orgAccess(w, org) {
			return true
		}
		switch r.Method {
		case http.MethodGet:
			s.getWebhook(w, org, id)
		case http.MethodPut:
			s.putWebhook(w, org, id, body)
		case http.MethodDelete:
			s.deleteWebhook(w, org, id)
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
		return true
	}
	if m := webhooksPathRE.FindStringSubmatch(path); m != nil {
		org := m[1]
		if !s.orgAccess(w, org) {
			return true
		}
		switch r.Method {
		case http.MethodGet:
			s.listWebhooks(w, r, org)
		case http.MethodPost:
			s.createWebhook(w, org, body)
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
		return true
	}
	return false
}

// validateWebhookEvents returns the one §2/§3 detail for a bad events list, or
// nil. Callers already know events is non-empty.
func validateWebhookEvents(events []string) map[string]any {
	if len(events) == 1 && events[0] == "*" {
		return nil
	}
	for _, e := range events {
		if e == "*" {
			return detail("events", "invalid", "wildcard '*' must be the only entry")
		}
		if !webhookCatalog[e] {
			return detail("events", "invalid", fmt.Sprintf("unknown event type %q", e))
		}
	}
	return nil
}

// blockedIPRanges are the CGNAT/broadcast/NAT64 ranges §2 names beyond what
// net.IP's own Is* methods already cover (loopback, private/ULA, link-local,
// multicast, unspecified).
var blockedIPRanges = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{"100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "255.255.255.255/32", "64:ff9b::/96"} {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(err) // constant, correct by construction
		}
		out = append(out, n)
	}
	return out
}()

func blockedWebhookIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4 // unmap an IPv4-mapped IPv6 literal (::ffff:10.0.0.1) before classifying it
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range blockedIPRanges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// validateWebhookURL is the §2 ValidateURL step: parse error, non-https
// scheme (the fake models WEBHOOK_ALLOW_HTTP=false, the resolved U1 default
// on both dev and prod), no host, embedded credentials, then a literal
// blocked-range IP. Hostnames are never resolved (§2, I3): "https://localhost/"
// passes here, exactly as it does against the real API.
func validateWebhookURL(raw string) map[string]any {
	u, err := url.Parse(raw)
	if err != nil {
		return detail("url", "invalid", "invalid url: "+err.Error())
	}
	if u.Scheme != "https" {
		return detail("url", "invalid", "url must use https")
	}
	if u.Host == "" {
		return detail("url", "invalid", "url must have a host")
	}
	if u.User != nil {
		return detail("url", "invalid", "url must not contain credentials")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && blockedWebhookIP(ip) {
		return detail("url", "invalid", "url host resolves to a blocked address range")
	}
	return nil
}

// createWebhook is POST /organizations/{org}/webhooks (§2).
func (s *Server) createWebhook(w http.ResponseWriter, org string, body []byte) {
	var req struct {
		URL         string   `json:"url"`
		Description *string  `json:"description"`
		Events      []string `json:"events"`
	}
	// No Enabled/Secret field on this struct: decodeStrict's
	// DisallowUnknownFields is what turns either one, present in the body,
	// into the measured 400 - a webhook can only be created enabled (§2).
	if !decodeStrict(body, &req) {
		apiError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	var details []map[string]any
	if req.URL == "" {
		details = append(details, detail("url", "required", "URL is required"))
	}
	if len(req.Events) == 0 {
		details = append(details, detail("events", "required", "At least one event is required"))
	}
	if details != nil {
		validationError(w, details)
		return
	}
	if d := validateWebhookEvents(req.Events); d != nil {
		validationError(w, []map[string]any{d})
		return
	}
	if d := validateWebhookURL(req.URL); d != nil {
		validationError(w, []map[string]any{d})
		return
	}

	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.init()
	if len(ws.byOrg[org]) >= webhookCap { // disabled webhooks count too (§2) - none can exist yet, every create yields enabled:true
		apiError(w, http.StatusConflict, "webhook_limit", "Webhook limit reached for this organization")
		return
	}
	description := ""
	if req.Description != nil {
		description = *req.Description
	}
	ws.seq++
	wh := &fakeWebhook{
		id: uuid.NewString(), orgID: org, url: req.URL, description: description,
		events: append([]string(nil), req.Events...), enabled: true, secret: newWebhookSecret(),
		createdSeq: ws.seq, updatedSeq: ws.seq,
	}
	ws.byOrg[org] = append(ws.byOrg[org], wh)
	writeJSON(w, http.StatusCreated, webhookJSON(org, wh, true))
}

// getWebhook is GET /organizations/{org}/webhooks/{id} (§4).
func (s *Server) getWebhook(w http.ResponseWriter, org, id string) {
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	wh, _ := ws.find(org, id)
	if wh == nil {
		apiError(w, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	writeJSON(w, http.StatusOK, webhookJSON(org, wh, false))
}

// putWebhook is PUT /organizations/{org}/webhooks/{id}, a COALESCE partial
// merge (§5).
func (s *Server) putWebhook(w http.ResponseWriter, org, id string, body []byte) {
	if f := s.webhooks.consumeFailure(s.webhooks.failNextPUT, org); f != nil {
		apiError(w, f.status, f.code, f.message)
		return
	}
	var req struct {
		URL         *string   `json:"url"`
		Description *string   `json:"description"`
		Events      *[]string `json:"events"`
		Enabled     *bool     `json:"enabled"`
	}
	// Unknown keys, `secret` included, are 400 (§5) - decodeStrict alone
	// enforces it, since this struct has no Secret field.
	if !decodeStrict(body, &req) {
		apiError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	var details []map[string]any
	if req.URL != nil {
		if d := validateWebhookURL(*req.URL); d != nil {
			details = append(details, d)
		}
	}
	if req.Events != nil {
		if len(*req.Events) == 0 {
			// A DIFFERENT message from create's "At least one event is
			// required" (§5) - deliberately not shared with the create path.
			details = append(details, detail("events", "invalid", "at least one event type is required"))
		} else if d := validateWebhookEvents(*req.Events); d != nil {
			details = append(details, d)
		}
	}
	if details != nil {
		validationError(w, details)
		return
	}

	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	wh, _ := ws.find(org, id)
	if wh == nil {
		apiError(w, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	if req.URL != nil {
		wh.url = *req.URL
	}
	if req.Description != nil {
		wh.description = *req.Description // "" is the only clear; omitted/null keeps
	}
	if req.Events != nil {
		wh.events = append([]string(nil), (*req.Events)...)
	}
	if req.Enabled != nil {
		wh.enabled = *req.Enabled
	}
	ws.seq++ // moves on EVERY successful PUT, `{}` included (§5)
	wh.updatedSeq = ws.seq
	writeJSON(w, http.StatusOK, webhookJSON(org, wh, false))
}

// rotateWebhookSecret is POST /organizations/{org}/webhooks/{id}/rotate-secret
// (§6): no body, always a brand new secret, allowed on a disabled webhook.
func (s *Server) rotateWebhookSecret(w http.ResponseWriter, org, id string) {
	if f := s.webhooks.consumeFailure(s.webhooks.failNextRotate, org); f != nil {
		apiError(w, f.status, f.code, f.message)
		return
	}
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	wh, _ := ws.find(org, id)
	if wh == nil {
		apiError(w, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	wh.secret = newWebhookSecret()
	ws.seq++
	wh.updatedSeq = ws.seq
	writeJSON(w, http.StatusOK, webhookJSON(org, wh, true))
}

// deleteWebhook is DELETE /organizations/{org}/webhooks/{id} (§7): a hard
// delete, 204 then a second DELETE is 404.
func (s *Server) deleteWebhook(w http.ResponseWriter, org, id string) {
	ws := &s.webhooks
	ws.mu.Lock()
	defer ws.mu.Unlock()
	wh, i := ws.find(org, id)
	if wh == nil {
		apiError(w, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	l := ws.byOrg[org]
	ws.byOrg[org] = append(l[:i:i], l[i+1:]...)
	w.WriteHeader(http.StatusNoContent)
}

// listWebhooks is GET /organizations/{org}/webhooks (§10): ordered by id,
// "id > cursor", limit clamped 1..100 (default 50), next_cursor set exactly
// when the page is full. The provider never calls this (it imports by id),
// so it exists only for the fake's own coverage that list also omits secret.
func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request, org string) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = min(max(n, 1), 100)
		}
	}
	cursor := r.URL.Query().Get("cursor")
	ws := &s.webhooks
	ws.mu.Lock()
	all := append([]*fakeWebhook(nil), ws.byOrg[org]...)
	ws.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	items := []map[string]any{}
	var last string
	for _, wh := range all {
		if cursor != "" && wh.id <= cursor {
			continue
		}
		if len(items) == limit {
			break
		}
		items = append(items, webhookJSON(org, wh, false))
		last = wh.id
	}
	out := map[string]any{"items": items}
	if len(items) == limit {
		out["next_cursor"] = last
	}
	writeJSON(w, http.StatusOK, out)
}
