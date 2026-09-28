package fakeapi

// Registry routes of the fake, reproducing what aether-api DOES (measured at
// aether-api 8e9045a; SDD task-11-measured-contract.md), not what its OpenAPI
// spec says. `/robot/rotate`, `/cleanup-policies/{id}/dry-run`,
// `/cleanup-policies/{id}/run` and `/repositories*` are OUT OF SCOPE (Task 11
// brief) and are not served here - a request to any of them falls through to
// the fake's normal 404 "no route" (none of the regexes below match their
// extra path segment).
//
//	GET    /organizations/{org}/registry                                200 Registry | {"enabled":false}
//	POST   /organizations/{org}/registry                                201 EnableRegistryResult | 409
//	DELETE /organizations/{org}/registry[?purge=true]                   204 (idempotent)
//	GET    /organizations/{org}/registry/robot-accounts[?cursor=]       200 {items[]} (NO next_cursor, fixed page 50)
//	POST   /organizations/{org}/registry/robot-accounts                 201 RobotAccountWithSecret (+harbor_username, +registry) | 400 | 409 | 422
//	DELETE /organizations/{org}/registry/robot-accounts/{id}            204 | 404 | 500 | 503 (sticky)
//	GET    /organizations/{org}/registry/cleanup-policies                200 {items[]} (unpaginated)
//	POST   /organizations/{org}/registry/cleanup-policies                201 CleanupPolicy (no last_run_*) | 400 | 500
//	PUT    /organizations/{org}/registry/cleanup-policies/{id}           200 CleanupPolicy (no last_run_*) | 500
//	DELETE /organizations/{org}/registry/cleanup-policies/{id}           204 | 500
//
// Behaviours that matter and that the fake copies (contract section numbers
// below):
//   - request decoding is LAX everywhere on this subtree (§1): an unknown
//     JSON key is silently ignored - the OPPOSITE of webhooks/members, which
//     is why this file uses decodeLax, never decodeStrict;
//   - GET /registry never 404s: a never-enabled or disabled org reads
//     `{"enabled":false}` (§2). enabled is always true on a real row - there
//     is no code path that stores it false;
//   - POST /registry enable checks for an existing robot literally named
//     "org-<slug>" BEFORE creating anything, whether or not a registry row
//     exists (§3): a leaked Harbor robot or a user robot with that name both
//     409 robot_exists even while GET says disabled - the fake models this
//     with ONE check against the robots list, not a registry-row flag,
//     because that is the actual wedge the real API produces;
//   - a successful enable creates the registry row AND a default robot row
//     "org-<slug>" with permissions
//     ["repository:push","repository:pull","repository:delete"] - a
//     vocabulary CreateRobotAccount itself refuses (§3/§6, T11-7's import
//     guard exists because of this row);
//   - DELETE /registry (disable) deletes the registry row AND every robot
//     account of the org, Terraform-managed ones included, and is idempotent
//     - a second DELETE, or one against a never-enabled org, is still 204
//     (§4). Only the literal query value "true" counts as purge - "1"/"True"/
//     anything else behaves as purge=false (§4, "Purge is a literal check");
//   - robot accounts do NOT require the registry to be enabled (§6): the
//     store is independent of the registry row;
//   - robot list is fixed at a 50-item page, ORDER BY id, an undocumented
//     `?cursor=<last id>` IS honoured, and there is NEVER a `next_cursor` key
//     in the body - a caller must keep paging until a page is short (§6);
//   - robot `permissions` are stored VERBATIM as sent - no sort, no dedupe, no
//     expansion of push->pull+push (§6);
//   - robot delete is Harbor-FIRST, not fail-forward (§7): a Harbor failure
//     (modelled here as the sticky override) answers 503 and KEEPS the row -
//     the opposite of DisableRegistry's best-effort robot cleanup;
//   - cleanup-policy create/update run NO validation beyond bad-JSON and (on
//     CREATE only) an empty name: an invalid/omitted `action` or `schedule`,
//     or an omitted `repo_scope`, is a 500 text/plain that mimics a DB CHECK/
//     NOT-NULL violation; `conditions` omitted stores JSON null with NO error
//     (§9/§11 - the one field that is allowed to go missing);
//   - cleanup PUT is a FULL REPLACE of name/action/conditions/repo_scope/
//     schedule (omitting one wipes/500s it, name has NO validation on PUT -
//     unlike create) and a COALESCE merge of `enabled` alone (§11);
//   - cleanup create/update responses NEVER carry last_run_* (the store
//     query does not select them, regardless of what is stored) - the fake
//     therefore has TWO render paths, not one render-omit-if-nil path (§9/
//     §11); the list DOES carry last_run_at/_deleted/_freed_bytes but NEVER
//     last_run_message, no matter what SetLastRun stored (§10);
//   - an unknown/foreign cleanup-policy id on PUT or DELETE is 500 text/plain,
//     never 404 (§11);
//   - text/plain errors are written with http.Error, exactly like the real
//     handler's http.Error(...) calls - never the JSON error envelope.
//
// Out-of-band knobs: Disable, SeedRobot/SeedRobotWithID/RemoveRobot,
// StickyRobotDeleteFailure, SeedPolicy/SetLastRun/RemovePolicy - see each
// doc comment.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	registryPathRE = regexp.MustCompile(`^/organizations/([^/]+)/registry$`)
	robotsPathRE   = regexp.MustCompile(`^/organizations/([^/]+)/registry/robot-accounts$`)
	robotPathRE    = regexp.MustCompile(`^/organizations/([^/]+)/registry/robot-accounts/([^/]+)$`)
	policiesPathRE = regexp.MustCompile(`^/organizations/([^/]+)/registry/cleanup-policies$`)
	policyPathRE   = regexp.MustCompile(`^/organizations/([^/]+)/registry/cleanup-policies/([^/]+)$`)
)

// registryHost models the resolved U1 dev value of HARBOR_REGISTRY_HOST. Only
// its presence/shape matters to a caller, never the literal string.
const registryHost = "dev.registry.aetherplatform.cloud"

const registryQuotaGB = 50

// defaultRobotPermissions is what EnableRegistry writes for the "org-<slug>"
// row - a vocabulary CreateRobotAccount itself rejects (§3/§6).
var defaultRobotPermissions = []string{"repository:push", "repository:pull", "repository:delete"}

const robotPageSize = 50

type fakeRegistry struct {
	id, orgID, harborProject, endpoint string
	seq                                int // created_at == updated_at always (no route ever updates a registry row, §2)
}

type fakeRobot struct {
	id, orgID, name       string
	permissions           []string // stored verbatim, order/duplicates kept (§6)
	harborID              int64
	harborUsername, token string
	seq                   int // created_at == updated_at always (no update route exists, §8)
}

type fakeCleanupPolicy struct {
	id, orgID, name, action, schedule string
	conditions                        []map[string]any // nil = JSON null; element order preserved
	repoScope                         []string
	enabled                           bool
	createdSeq, updatedSeq            int
	lastRunSet                        bool // false until a run: last_run_* keys are omitted, not merely null
	lastRunDeleted                    int
	lastRunFreedBytes                 int64
	lastRunMessage                    string // stored but NEVER rendered (§10) - kept only so SetLastRun's signature is complete
}

// registryFailure is a STICKY (never auto-cleared) one, unlike webhookStore's
// one-shot overrides - the retrying transport makes up to 4 attempts (1 +
// maxRetries) at a DELETE, and a real Harbor outage does not resolve itself
// between them.
type registryFailure struct {
	status        int
	code, message string
}

// registryStore is embedded by value in Server; its zero value is usable.
type registryStore struct {
	mu sync.Mutex

	registries map[string]*fakeRegistry        // by org; absent = disabled/never enabled
	robots     map[string][]*fakeRobot         // by org
	policies   map[string][]*fakeCleanupPolicy // by org

	seq         int   // monotonic clock shared by every entity this file writes
	harborIDSeq int64 // next Harbor-assigned robot id

	// robotDeleteFail is keyed "org/id"; see StickyRobotDeleteFailure.
	robotDeleteFail map[string]*registryFailure

	// purgeCount counts DELETEs of org's registry whose query carried the
	// LITERAL value purge=true - see PurgeCount. Repositories are out of
	// scope (nothing is actually purged), so this counter is the only
	// observable trace of "only the literal true counts" (§4).
	purgeCount map[string]int
}

func (rs *registryStore) init() {
	if rs.registries == nil {
		rs.registries = map[string]*fakeRegistry{}
	}
	if rs.robots == nil {
		rs.robots = map[string][]*fakeRobot{}
	}
	if rs.policies == nil {
		rs.policies = map[string][]*fakeCleanupPolicy{}
	}
}

func (rs *registryStore) findRobot(org, id string) (*fakeRobot, int) {
	for i, r := range rs.robots[org] {
		if r.id == id {
			return r, i
		}
	}
	return nil, -1
}

func (rs *registryStore) findPolicy(org, id string) (*fakeCleanupPolicy, int) {
	for i, p := range rs.policies[org] {
		if p.id == id {
			return p, i
		}
	}
	return nil, -1
}

// registryEpoch anchors every timestamp this file writes; only relative
// movement and created_at == updated_at (where the contract says so) matter.
var registryEpoch = time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

func registryTime(seq int) string {
	return registryEpoch.Add(time.Duration(seq) * time.Second).Format(time.RFC3339)
}

func newRegistrySecret(prefix string) string {
	return prefix + uuid.NewString() + uuid.NewString()
}

// orgSlug returns the org's slug, or "" if it is not registered. Unlike
// orgByID (clusters.go), this takes its own lock - every caller here already
// released s.mu by the time it needs the slug.
func (s *Server) orgSlug(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orgs {
		if o.ID == id {
			return o.Slug
		}
	}
	return ""
}

func harborProjectOf(slug string) string { return "aether-" + slug }

func harborUsernameOf(slug, name string) string {
	return "robot$" + harborProjectOf(slug) + "+" + name
}

func defaultRobotName(slug string) string { return "org-" + slug }

// decodeLax is the registry subtree's decoder (§1): unlike decodeStrict, it
// does NOT set DisallowUnknownFields - unknown keys are silently ignored,
// matching every registry/robot/cleanup handler's plain
// json.NewDecoder(r.Body).Decode.
func decodeLax(body []byte, v any) bool {
	return json.Unmarshal(body, v) == nil
}

// ---------------------------------------------------------------- knobs

// Disable removes org's registry row AND every robot account of the org, out
// of band (no request recorded) - what a real DELETE /registry does, or what
// another caller (the portal, another token) could do without this test's
// Terraform run seeing it. Used to simulate the registry going away between
// two plans.
func (s *Server) Disable(org string) {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	delete(rs.registries, org)
	delete(rs.robots, org)
}

// SeedRobot adds a robot account out of band (no request recorded, no
// validation - it can store permissions CreateRobotAccount would refuse) and
// returns its id.
func (s *Server) SeedRobot(org, name string, permissions []string) string {
	return s.SeedRobotWithID(org, uuid.NewString(), name, permissions)
}

// SeedRobotWithID is SeedRobot with a caller-chosen id, so a test can control
// where the row sorts in the id-ordered, 50-per-page robot list (a pagination
// test needs its target on a specific page).
func (s *Server) SeedRobotWithID(org, id, name string, permissions []string) string {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	rs.seq++
	rs.harborIDSeq++
	slug := s.orgSlugLocked(org)
	r := &fakeRobot{
		id: id, orgID: org, name: name,
		permissions: append([]string(nil), permissions...),
		harborID:    rs.harborIDSeq, harborUsername: harborUsernameOf(slug, name),
		token: newRegistrySecret("hbrsec_"), seq: rs.seq,
	}
	rs.robots[org] = append(rs.robots[org], r)
	return id
}

// orgSlugLocked is orgSlug for a caller that must not re-take s.mu re-entrantly
// through orgSlug's own locking - registryStore.mu and Server.mu are already
// disjoint, so this only exists to avoid a second full org-list scan call site
// reading oddly; it is identical to orgSlug.
func (s *Server) orgSlugLocked(org string) string { return s.orgSlug(org) }

// RemoveRobot deletes a robot account out of band (no request recorded). It
// reports whether the robot existed.
func (s *Server) RemoveRobot(org, id string) bool {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	_, i := rs.findRobot(org, id)
	if i < 0 {
		return false
	}
	l := rs.robots[org]
	rs.robots[org] = append(l[:i:i], l[i+1:]...)
	return true
}

// StickyRobotDeleteFailure makes EVERY DELETE of org's robot id answer
// (status, code, message) instead of deleting it, until this is called again
// with a different id (or the server is discarded). Unlike webhookStore's
// one-shot overrides, this does NOT auto-clear: the retrying transport
// retries a DELETE up to 3 times (TR:148-157), and a one-shot override would
// only fail the first attempt, letting the retry silently succeed - masking
// exactly the "Harbor stays down across every retry" case this exists to
// test (contract §7's 503, row kept).
func (s *Server) StickyRobotDeleteFailure(org, id string, status int, code, message string) {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.robotDeleteFail == nil {
		rs.robotDeleteFail = map[string]*registryFailure{}
	}
	rs.robotDeleteFail[org+"/"+id] = &registryFailure{status: status, code: code, message: message}
}

// SeedPolicy adds a cleanup policy out of band (no request recorded, no
// validation) and returns its id. conditions is stored EXACTLY as given - a
// nil slice reads back as JSON null, and each map's value can be a string,
// float64, bool or absent, reproducing what a non-Terraform client (or a
// legacy row) can leave behind (§9 U3, e.g. {"type":"tag_count","op":"gt",
// "value":10} or the fail-open portal shape {"type":"untagged"}).
func (s *Server) SeedPolicy(org, name, action string, conditions []map[string]any, repoScope []string, schedule string, enabled bool) string {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	rs.seq++
	p := &fakeCleanupPolicy{
		id: uuid.NewString(), orgID: org, name: name, action: action,
		conditions: conditions, repoScope: append([]string(nil), repoScope...),
		schedule: schedule, enabled: enabled, createdSeq: rs.seq, updatedSeq: rs.seq,
	}
	rs.policies[org] = append(rs.policies[org], p)
	return p.id
}

// SetLastRun records a completed run's result out of band (as the 60s
// scheduler or POST .../run would), advancing updated_at exactly as a real
// run does (§12). It reports whether the policy exists.
func (s *Server) SetLastRun(org, id string, deleted int, freedBytes int64, message string) bool {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	p, _ := rs.findPolicy(org, id)
	if p == nil {
		return false
	}
	rs.seq++
	p.lastRunSet = true
	p.lastRunDeleted, p.lastRunFreedBytes, p.lastRunMessage = deleted, freedBytes, message
	p.updatedSeq = rs.seq
	return true
}

// PurgeCount returns how many DELETE /registry calls for org carried the
// LITERAL query value purge=true (§4: "1", "True" and anything else other
// than the exact lower-case string do not count).
func (s *Server) PurgeCount(org string) int {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.purgeCount[org]
}

// RemovePolicy deletes a cleanup policy out of band (no request recorded). It
// reports whether the policy existed.
func (s *Server) RemovePolicy(org, id string) bool {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	_, i := rs.findPolicy(org, id)
	if i < 0 {
		return false
	}
	l := rs.policies[org]
	rs.policies[org] = append(l[:i:i], l[i+1:]...)
	return true
}

// ---------------------------------------------------------------- routing

// routeRegistry serves the registry/robot-account/cleanup-policy routes; it
// reports false when path is not one of them. /robot/rotate, the cleanup
// dry-run/run actions and /repositories* are out of scope and fall through to
// the caller's default 404 (none of the regexes above match their extra path
// segment).
func (s *Server) routeRegistry(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	if m := robotPathRE.FindStringSubmatch(path); m != nil {
		org, id := m[1], m[2]
		if !s.orgAccess(w, org) {
			return true
		}
		if r.Method != http.MethodDelete {
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
			return true
		}
		s.deleteRobotAccount(w, org, id)
		return true
	}
	if m := robotsPathRE.FindStringSubmatch(path); m != nil {
		org := m[1]
		if !s.orgAccess(w, org) {
			return true
		}
		switch r.Method {
		case http.MethodGet:
			s.listRobotAccounts(w, r, org)
		case http.MethodPost:
			s.createRobotAccount(w, org, body)
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
		return true
	}
	if m := policyPathRE.FindStringSubmatch(path); m != nil {
		org, id := m[1], m[2]
		if !s.orgAccess(w, org) {
			return true
		}
		switch r.Method {
		case http.MethodPut:
			s.putCleanupPolicy(w, org, id, body)
		case http.MethodDelete:
			s.deleteCleanupPolicy(w, org, id)
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
		return true
	}
	if m := policiesPathRE.FindStringSubmatch(path); m != nil {
		org := m[1]
		if !s.orgAccess(w, org) {
			return true
		}
		switch r.Method {
		case http.MethodGet:
			s.listCleanupPolicies(w, org)
		case http.MethodPost:
			s.createCleanupPolicy(w, org, body)
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
		return true
	}
	if m := registryPathRE.FindStringSubmatch(path); m != nil {
		org := m[1]
		if !s.orgAccess(w, org) {
			return true
		}
		switch r.Method {
		case http.MethodGet:
			s.getRegistry(w, org)
		case http.MethodPost:
			s.enableRegistry(w, org)
		case http.MethodDelete:
			s.disableRegistry(w, r, org)
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
		return true
	}
	return false
}

// ---------------------------------------------------------------- registry

func registryJSON(reg *fakeRegistry) map[string]any {
	return map[string]any{
		"id": reg.id, "org_id": reg.orgID, "harbor_project": reg.harborProject,
		"endpoint": reg.endpoint, "enabled": true, "quota_gb": registryQuotaGB,
		"created_at": registryTime(reg.seq), "updated_at": registryTime(reg.seq),
		// used_gb is NEVER included: nothing writes it (§2).
	}
}

// getRegistry is GET /organizations/{org}/registry (§2): never 404s.
func (s *Server) getRegistry(w http.ResponseWriter, org string) {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	reg, ok := rs.registries[org]
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, registryJSON(reg))
}

// enableRegistry is POST /organizations/{org}/registry (§3): no body.
func (s *Server) enableRegistry(w http.ResponseWriter, org string) {
	slug := s.orgSlug(org)
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	defaultName := defaultRobotName(slug)
	for _, r := range rs.robots[org] {
		if r.name == defaultName {
			// Either the default robot survived a best-effort disable, or a
			// user created a robot named "org-<slug>" through
			// /robot-accounts - both cases 409 the SAME way the real Harbor
			// project-robot create does, whether or not a registry row
			// exists (§3, "other ways to reach that 409 while GET says
			// disabled").
			apiError(w, http.StatusConflict, "robot_exists", "registry already enabled; use rotate")
			return
		}
	}
	rs.seq++
	rs.harborIDSeq++
	project := harborProjectOf(slug)
	reg := &fakeRegistry{id: uuid.NewString(), orgID: org, harborProject: project, endpoint: registryHost + "/" + project, seq: rs.seq}
	rs.registries[org] = reg
	robot := &fakeRobot{
		id: uuid.NewString(), orgID: org, name: defaultName,
		permissions: append([]string(nil), defaultRobotPermissions...),
		harborID:    rs.harborIDSeq, harborUsername: harborUsernameOf(slug, defaultName),
		token: newRegistrySecret("hbrsec_"), seq: rs.seq,
	}
	rs.robots[org] = append(rs.robots[org], robot)
	writeJSON(w, http.StatusCreated, map[string]any{
		"registry":    registryJSON(reg),
		"credentials": map[string]any{"username": robot.harborUsername, "password": robot.token},
	})
}

// disableRegistry is DELETE /organizations/{org}/registry[?purge=true] (§4):
// idempotent, deletes the registry row and EVERY robot of the org. Only the
// literal query value "true" counts as purge; repositories are not modelled
// (out of scope), so a caller can only observe the query value itself.
func (s *Server) disableRegistry(w http.ResponseWriter, r *http.Request, org string) {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	if r.URL.Query().Get("purge") == "true" { // literal check only (§4) - see PurgeCount
		if rs.purgeCount == nil {
			rs.purgeCount = map[string]int{}
		}
		rs.purgeCount[org]++
	}
	delete(rs.registries, org)
	delete(rs.robots, org)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- robot accounts

func robotJSON(robot *fakeRobot) map[string]any {
	return map[string]any{
		"id": robot.id, "org_id": robot.orgID, "name": robot.name,
		"permissions": append([]string{}, robot.permissions...),
		"harbor_id":   robot.harborID, "harbor_username": robot.harborUsername,
		"created_at": registryTime(robot.seq), "updated_at": registryTime(robot.seq),
		// last_used_at is NEVER included: nothing writes it (§6).
	}
}

// createRobotAccount is POST /organizations/{org}/registry/robot-accounts
// (§6). Robots do not require the registry to be enabled.
func (s *Server) createRobotAccount(w http.ResponseWriter, org string, body []byte) {
	var req struct {
		Name        string    `json:"name"`
		Permissions *[]string `json:"permissions"`
	}
	if !decodeLax(body, &req) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	var perms []string
	if req.Permissions != nil {
		perms = *req.Permissions
	}
	if len(perms) == 0 {
		validationError(w, []map[string]any{detail("permissions", "required", "at least one permission is required")})
		return
	}
	for _, p := range perms {
		if p != "push" && p != "pull" && p != "delete" {
			validationError(w, []map[string]any{detail("permissions", "invalid", "permissions may contain only push, pull, delete")})
			return
		}
	}
	slug := s.orgSlug(org)
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	for _, r := range rs.robots[org] {
		if r.name == req.Name {
			apiError(w, http.StatusConflict, "robot_exists", "a robot account with that name already exists")
			return
		}
	}
	rs.seq++
	rs.harborIDSeq++
	robot := &fakeRobot{
		id: uuid.NewString(), orgID: org, name: req.Name,
		permissions: append([]string(nil), perms...), // verbatim: no sort, no dedupe (§6)
		harborID:    rs.harborIDSeq, harborUsername: harborUsernameOf(slug, req.Name),
		token: newRegistrySecret("hbrsec_"), seq: rs.seq,
	}
	rs.robots[org] = append(rs.robots[org], robot)
	m := robotJSON(robot)
	m["token"] = robot.token
	m["registry"] = registryHost // bare host, no project suffix (§6) - absent from OA/CG, only reachable by decoding the raw body
	writeJSON(w, http.StatusCreated, m)
}

// listRobotAccounts is GET /organizations/{org}/registry/robot-accounts
// (§6): fixed 50-item page, ORDER BY id, an undocumented `?cursor=<last id>`
// honoured, and NEVER a next_cursor key - the provider must page until a
// short page.
func (s *Server) listRobotAccounts(w http.ResponseWriter, r *http.Request, org string) {
	cursor := r.URL.Query().Get("cursor")
	rs := &s.registries
	rs.mu.Lock()
	all := append([]*fakeRobot(nil), rs.robots[org]...)
	rs.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })
	items := []map[string]any{}
	for _, robot := range all {
		if cursor != "" && robot.id <= cursor {
			continue
		}
		if len(items) == robotPageSize {
			break
		}
		items = append(items, robotJSON(robot))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items}) // deliberately NO next_cursor (§6)
}

// deleteRobotAccount is DELETE .../robot-accounts/{id} (§7): Harbor-first, NOT
// fail-forward. A malformed id (not a UUID) mirrors pgx's cast failure -> 500;
// a well-formed unknown id is 404.
func (s *Server) deleteRobotAccount(w http.ResponseWriter, org, id string) {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	robot, i := rs.findRobot(org, id)
	if robot == nil {
		if _, err := uuid.Parse(id); err != nil {
			http.Error(w, fmt.Sprintf("store_error: invalid input syntax for type uuid: %q", id), http.StatusInternalServerError)
			return
		}
		apiError(w, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	if f, ok := rs.robotDeleteFail[org+"/"+id]; ok {
		apiError(w, f.status, f.code, f.message)
		return
	}
	l := rs.robots[org]
	rs.robots[org] = append(l[:i:i], l[i+1:]...)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- cleanup policies

// policyBaseJSON renders every field the store selects on EVERY read path
// (§9/§10/§11) except last_run_*, which create/update never select at all
// (see policyJSONWithLastRun for the one path that does).
func policyBaseJSON(p *fakeCleanupPolicy) map[string]any {
	var conditions any // nil -> JSON null, matching a stored nil slice (§9)
	if p.conditions != nil {
		conditions = p.conditions
	}
	return map[string]any{
		"id": p.id, "org_id": p.orgID, "name": p.name, "action": p.action,
		"conditions": conditions, "repo_scope": append([]string{}, p.repoScope...),
		"schedule": p.schedule, "enabled": p.enabled,
		"created_at": registryTime(p.createdSeq), "updated_at": registryTime(p.updatedSeq),
	}
}

// policyListJSON is the list's render (§10): carries last_run_at/_deleted/
// _freed_bytes when a run has happened, but NEVER last_run_message.
func policyListJSON(p *fakeCleanupPolicy) map[string]any {
	m := policyBaseJSON(p)
	if p.lastRunSet {
		m["last_run_at"] = registryTime(p.updatedSeq)
		m["last_run_deleted"] = p.lastRunDeleted
		m["last_run_freed_bytes"] = p.lastRunFreedBytes
	}
	return m
}

// validCleanupAction/-Schedule are the DB CHECK constraints the explicit
// INSERT/UPDATE column list defeats any DEFAULT for (§9/§11).
func validCleanupAction(s string) bool   { return s == "delete" || s == "protect" }
func validCleanupSchedule(s string) bool { return s == "nightly" || s == "weekly" || s == "manual" }

type cleanupPolicyRequest struct {
	Name       string            `json:"name"`
	Action     *string           `json:"action"`
	Conditions *[]map[string]any `json:"conditions"`
	RepoScope  *[]string         `json:"repo_scope"`
	Schedule   *string           `json:"schedule"`
	Enabled    *bool             `json:"enabled"`
}

// createCleanupPolicy is POST .../cleanup-policies (§9): the ONLY checks are
// bad JSON and an empty name; everything else goes straight to an explicit-
// column INSERT that a DB CHECK/NOT-NULL can reject.
func (s *Server) createCleanupPolicy(w http.ResponseWriter, org string, body []byte) {
	var req cleanupPolicyRequest
	if !decodeLax(body, &req) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	action := ""
	if req.Action != nil {
		action = *req.Action
	}
	if !validCleanupAction(action) {
		http.Error(w, `create cleanup policy: ERROR: new row for relation "cleanup_policies" violates check constraint "cleanup_policies_action_check"`, http.StatusInternalServerError)
		return
	}
	schedule := ""
	if req.Schedule != nil {
		schedule = *req.Schedule
	}
	if !validCleanupSchedule(schedule) {
		http.Error(w, `create cleanup policy: ERROR: new row for relation "cleanup_policies" violates check constraint "cleanup_policies_schedule_check"`, http.StatusInternalServerError)
		return
	}
	if req.RepoScope == nil {
		http.Error(w, `create cleanup policy: ERROR: null value in column "repo_scope" of relation "cleanup_policies" violates not-null constraint`, http.StatusInternalServerError)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	var conditions []map[string]any
	if req.Conditions != nil {
		conditions = *req.Conditions
	}
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	rs.seq++
	p := &fakeCleanupPolicy{
		id: uuid.NewString(), orgID: org, name: req.Name, action: action,
		conditions: conditions, repoScope: append([]string(nil), (*req.RepoScope)...),
		schedule: schedule, enabled: enabled, createdSeq: rs.seq, updatedSeq: rs.seq,
	}
	rs.policies[org] = append(rs.policies[org], p)
	writeJSON(w, http.StatusCreated, policyBaseJSON(p)) // no last_run_* (§9)
}

func (s *Server) listCleanupPolicies(w http.ResponseWriter, org string) {
	rs := &s.registries
	rs.mu.Lock()
	all := append([]*fakeCleanupPolicy(nil), rs.policies[org]...)
	rs.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].createdSeq < all[j].createdSeq })
	items := make([]map[string]any, 0, len(all))
	for _, p := range all {
		items = append(items, policyListJSON(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items}) // unpaginated (§10)
}

// putCleanupPolicy is PUT .../cleanup-policies/{id} (§11): a FULL REPLACE of
// every field but `enabled` (merged), an unknown/foreign id is 500 (never
// 404), and - unlike create - `name` is NOT validated here at all.
func (s *Server) putCleanupPolicy(w http.ResponseWriter, org, id string, body []byte) {
	var req cleanupPolicyRequest
	if !decodeLax(body, &req) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	action := ""
	if req.Action != nil {
		action = *req.Action
	}
	if !validCleanupAction(action) {
		http.Error(w, `update cleanup policy: ERROR: new row for relation "cleanup_policies" violates check constraint "cleanup_policies_action_check"`, http.StatusInternalServerError)
		return
	}
	schedule := ""
	if req.Schedule != nil {
		schedule = *req.Schedule
	}
	if !validCleanupSchedule(schedule) {
		http.Error(w, `update cleanup policy: ERROR: new row for relation "cleanup_policies" violates check constraint "cleanup_policies_schedule_check"`, http.StatusInternalServerError)
		return
	}
	if req.RepoScope == nil {
		http.Error(w, `update cleanup policy: ERROR: null value in column "repo_scope" of relation "cleanup_policies" violates not-null constraint`, http.StatusInternalServerError)
		return
	}
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	p, _ := rs.findPolicy(org, id)
	if p == nil {
		http.Error(w, "update cleanup policy: no rows in result set", http.StatusInternalServerError)
		return
	}
	var conditions []map[string]any
	if req.Conditions != nil {
		conditions = *req.Conditions
	}
	p.name = req.Name // "" on an omitted name - NOT validated on PUT (§11)
	p.action = action
	p.schedule = schedule
	p.conditions = conditions // nil wipes to JSON null - no error (§11)
	p.repoScope = append([]string(nil), (*req.RepoScope)...)
	if req.Enabled != nil {
		p.enabled = *req.Enabled // the ONLY merged field
	}
	rs.seq++
	p.updatedSeq = rs.seq
	writeJSON(w, http.StatusOK, policyBaseJSON(p)) // no last_run_* (§11)
}

// deleteCleanupPolicy is DELETE .../cleanup-policies/{id} (§11): hard delete,
// an unknown/foreign/already-deleted id is 500, never 404.
func (s *Server) deleteCleanupPolicy(w http.ResponseWriter, org, id string) {
	rs := &s.registries
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.init()
	_, i := rs.findPolicy(org, id)
	if i < 0 {
		http.Error(w, "cleanup policy not found", http.StatusInternalServerError)
		return
	}
	l := rs.policies[org]
	rs.policies[org] = append(l[:i:i], l[i+1:]...)
	w.WriteHeader(http.StatusNoContent)
}
