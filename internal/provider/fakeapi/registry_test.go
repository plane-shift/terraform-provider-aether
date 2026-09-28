package fakeapi

// The fake's own tests for registry.go (SDD task-11-measured-contract.md).
// testOrg/newClusterFake/call/decode/errCode/detailsOf are shared helpers
// from clusters_test.go, members_test.go and nodepools_test.go (same
// package). testOrg's slug is "acme", so every expected harbor_project/
// endpoint/harbor_username below is literal "aether-acme"/"org-acme".

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const (
	registryPath = "/organizations/" + testOrg + "/registry"
	robotsPath   = registryPath + "/robot-accounts"
	policiesPath = registryPath + "/cleanup-policies"
)

func enableRegistryFor(t *testing.T, s *Server) map[string]any {
	t.Helper()
	resp, b := call(t, s, http.MethodPost, registryPath, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enable = %d %s, want 201", resp.StatusCode, b)
	}
	return decode(t, b)
}

// ---------------------------------------------------------------- R9: registry

// Fails if the disabled shape carries any key besides "enabled":false, or if
// a never-enabled org gets a 404 instead (§2).
func TestRegistryGetDisabledShapeIsExactlyEnabledFalse(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodGet, registryPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET disabled = %d %s, want 200", resp.StatusCode, b)
	}
	m := decode(t, b)
	if len(m) != 1 || m["enabled"] != false {
		t.Fatalf("body = %v, want exactly {\"enabled\":false}", m)
	}
}

// Fails if enable does not create the registry row with the measured fields,
// does not create the default "org-<slug>" robot with repository:* wildcard
// permissions, or the credentials don't match that robot's own Harbor
// identity (§3).
func TestRegistryEnableCreatesRegistryAndDefaultRobot(t *testing.T) {
	s := newClusterFake(t)
	m := enableRegistryFor(t, s)
	reg, _ := m["registry"].(map[string]any)
	creds, _ := m["credentials"].(map[string]any)
	if reg == nil || creds == nil {
		t.Fatalf("201 body = %v, want registry+credentials", m)
	}
	if reg["enabled"] != true || reg["harbor_project"] != "aether-acme" ||
		reg["endpoint"] != registryHost+"/aether-acme" || reg["quota_gb"] != float64(registryQuotaGB) {
		t.Fatalf("registry = %v", reg)
	}
	if _, has := reg["used_gb"]; has {
		t.Fatalf("registry leaked used_gb: %v", reg)
	}
	wantUser := "robot$aether-acme+org-acme"
	if creds["username"] != wantUser || creds["password"] == "" {
		t.Fatalf("credentials = %v, want username %q and a non-empty password", creds, wantUser)
	}

	_, lb := call(t, s, http.MethodGet, robotsPath, "")
	items := decode(t, lb)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("robot list = %v, want exactly the default robot", items)
	}
	robot := items[0].(map[string]any)
	if robot["name"] != "org-acme" || robot["harbor_username"] != wantUser {
		t.Fatalf("default robot = %v", robot)
	}
	perms := toStrings(robot["permissions"])
	want := []string{"repository:push", "repository:pull", "repository:delete"}
	if len(perms) != len(want) {
		t.Fatalf("default robot permissions = %v, want %v", perms, want)
	}
	for i, p := range want {
		if perms[i] != p {
			t.Fatalf("default robot permissions = %v, want %v", perms, want)
		}
	}
	if _, has := robot["token"]; has {
		t.Fatalf("robot list leaked token: %v", robot)
	}
}

// Fails if a second enable is anything but 409 robot_exists with the exact
// measured message, or if it re-mints credentials (nothing must change).
func TestRegistrySecondEnableIs409RobotExists(t *testing.T) {
	s := newClusterFake(t)
	enableRegistryFor(t, s)
	resp, b := call(t, s, http.MethodPost, registryPath, "")
	m := decode(t, b)
	if resp.StatusCode != http.StatusConflict || errCode(m) != "robot_exists" ||
		m["error"].(map[string]any)["message"] != "registry already enabled; use rotate" {
		t.Fatalf("second enable = %d %v, want 409 robot_exists \"registry already enabled; use rotate\"", resp.StatusCode, m)
	}
}

// Fails if enable does not also 409 when a user robot happens to be named
// "org-<slug>" while the registry itself reads disabled - the exact wedge
// §3 documents ("other ways to reach that 409 while GET says disabled").
func TestRegistryEnableConflictsOnLeakedDefaultRobotNameEvenWhileDisabled(t *testing.T) {
	s := newClusterFake(t)
	s.SeedRobot(testOrg, "org-acme", []string{"push"})

	if resp, b := call(t, s, http.MethodGet, registryPath, ""); decode(t, b)["enabled"] != false {
		t.Fatalf("GET = %d %s, want disabled before the enable attempt", resp.StatusCode, b)
	}
	resp, b := call(t, s, http.MethodPost, registryPath, "")
	m := decode(t, b)
	if resp.StatusCode != http.StatusConflict || errCode(m) != "robot_exists" {
		t.Fatalf("enable with a leaked org-acme robot = %d %v, want 409 robot_exists", resp.StatusCode, m)
	}
	if resp, b := call(t, s, http.MethodGet, registryPath, ""); decode(t, b)["enabled"] != false {
		t.Fatalf("GET after the failed enable = %d %s, want still disabled (nothing created)", resp.StatusCode, b)
	}
}

// Fails if disable is not idempotent (never-enabled, or a second call), or if
// it does not delete EVERY robot of the org, including one created through
// /robot-accounts (§4).
func TestRegistryDisableIsIdempotentAndDeletesAllRobots(t *testing.T) {
	s := newClusterFake(t)
	// Idempotent even when never enabled.
	if resp, b := call(t, s, http.MethodDelete, registryPath, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable never-enabled = %d %s, want 204", resp.StatusCode, b)
	}

	enableRegistryFor(t, s)
	call(t, s, http.MethodPost, robotsPath, `{"name":"ci","permissions":["push","pull"]}`)

	resp, b := call(t, s, http.MethodDelete, registryPath, "")
	if resp.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("disable = %d %q, want 204 empty", resp.StatusCode, b)
	}
	if _, b := call(t, s, http.MethodGet, registryPath, ""); decode(t, b)["enabled"] != false {
		t.Fatalf("GET after disable = %s, want disabled", b)
	}
	_, lb := call(t, s, http.MethodGet, robotsPath, "")
	if items := decode(t, lb)["items"].([]any); len(items) != 0 {
		t.Fatalf("robots after disable = %v, want none (both the default and the user-created robot must be gone)", items)
	}
	// Second disable: still 204.
	if resp, _ := call(t, s, http.MethodDelete, registryPath, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("second disable = %d, want 204", resp.StatusCode)
	}
}

// Fails if PurgeCount increments on anything but the literal query value
// "true" (§4).
func TestRegistryDisablePurgeCountsOnlyLiteralTrue(t *testing.T) {
	s := newClusterFake(t)
	enableRegistryFor(t, s)
	if resp, _ := call(t, s, http.MethodDelete, registryPath+"?purge=1", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable ?purge=1 status unexpected")
	}
	if n := s.PurgeCount(testOrg); n != 0 {
		t.Fatalf("PurgeCount after ?purge=1 = %d, want 0 (not the literal \"true\")", n)
	}
	enableRegistryFor(t, s)
	if resp, _ := call(t, s, http.MethodDelete, registryPath+"?purge=True", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable ?purge=True status unexpected")
	}
	if n := s.PurgeCount(testOrg); n != 0 {
		t.Fatalf("PurgeCount after ?purge=True = %d, want 0 (case must match exactly)", n)
	}
	enableRegistryFor(t, s)
	if resp, _ := call(t, s, http.MethodDelete, registryPath+"?purge=true", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable ?purge=true status unexpected")
	}
	if n := s.PurgeCount(testOrg); n != 1 {
		t.Fatalf("PurgeCount after ?purge=true = %d, want 1", n)
	}
}

// ---------------------------------------------------------------- B10: robot accounts

func toStrings(v any) []string {
	items, _ := v.([]any)
	out := make([]string, len(items))
	for i, it := range items {
		out[i], _ = it.(string)
	}
	return out
}

// Fails if an empty name is anything but 400 text/plain "name is required" -
// the API's ONE 400 on this route, despite OA documenting 422 (§6).
func TestRobotAccountCreateEmptyNameIs400TextPlain(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodPost, robotsPath, `{"name":"","permissions":["push"]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if !strings.Contains(string(b), "name is required") {
		t.Fatalf("body = %q, want it to contain \"name is required\"", b)
	}
}

// Fails if absent/empty permissions is anything but 422
// permissions/required/"at least one permission is required" (§6).
func TestRobotAccountCreateMissingPermissionsIs422Required(t *testing.T) {
	for _, body := range []string{`{"name":"ci"}`, `{"name":"ci","permissions":[]}`, `{"name":"ci","permissions":null}`} {
		s := newClusterFake(t)
		resp, b := call(t, s, http.MethodPost, robotsPath, body)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("body %s: status = %d, want 422", body, resp.StatusCode)
		}
		d := detailsOf(decode(t, b))
		if len(d) != 1 {
			t.Fatalf("body %s: details = %v, want exactly one", body, d)
		}
		f := d[0].(map[string]any)
		if f["field"] != "permissions" || f["code"] != "required" || f["message"] != "at least one permission is required" {
			t.Fatalf("body %s: detail = %v", body, f)
		}
	}
}

// Fails if a permission outside {push,pull,delete} is accepted, or its 422
// carries the wrong code/message (§6).
func TestRobotAccountCreateInvalidPermissionIs422Invalid(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodPost, robotsPath, `{"name":"ci","permissions":["push","admin"]}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	d := detailsOf(decode(t, b))
	if len(d) != 1 {
		t.Fatalf("details = %v, want exactly one", d)
	}
	f := d[0].(map[string]any)
	if f["field"] != "permissions" || f["code"] != "invalid" || f["message"] != "permissions may contain only push, pull, delete" {
		t.Fatalf("detail = %v", f)
	}
}

// Fails if a duplicate name is accepted instead of 409 robot_exists with the
// exact measured message (§6).
func TestRobotAccountCreateDuplicateNameIs409(t *testing.T) {
	s := newClusterFake(t)
	call(t, s, http.MethodPost, robotsPath, `{"name":"ci","permissions":["push"]}`)
	resp, b := call(t, s, http.MethodPost, robotsPath, `{"name":"ci","permissions":["pull"]}`)
	m := decode(t, b)
	if resp.StatusCode != http.StatusConflict || errCode(m) != "robot_exists" ||
		m["error"].(map[string]any)["message"] != "a robot account with that name already exists" {
		t.Fatalf("duplicate name = %d %v, want 409 robot_exists \"a robot account with that name already exists\"", resp.StatusCode, m)
	}
}

// Fails if permissions are sorted, de-duplicated or expanded (push must NOT
// imply pull in the stored value), if harbor_username/registry/token are
// missing from the raw create response, or if a robot can be created without
// the registry ever being enabled (§6).
func TestRobotAccountCreateStoresPermissionsVerbatimAndReturnsRawFields(t *testing.T) {
	s := newClusterFake(t)
	// No enable call at all: robots do not require it.
	resp, b := call(t, s, http.MethodPost, robotsPath, `{"name":"ci","permissions":["push","push","pull"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create without an enabled registry = %d %s, want 201", resp.StatusCode, b)
	}
	m := decode(t, b)
	perms := toStrings(m["permissions"])
	want := []string{"push", "push", "pull"}
	if len(perms) != len(want) {
		t.Fatalf("permissions = %v, want %v verbatim (order and duplicates kept)", perms, want)
	}
	for i, p := range want {
		if perms[i] != p {
			t.Fatalf("permissions = %v, want %v verbatim", perms, want)
		}
	}
	wantUser := "robot$aether-acme+ci"
	if m["harbor_username"] != wantUser {
		t.Fatalf("harbor_username = %v, want %q", m["harbor_username"], wantUser)
	}
	if m["registry"] != registryHost {
		t.Fatalf("registry = %v, want the bare host %q (no project suffix)", m["registry"], registryHost)
	}
	token, _ := m["token"].(string)
	if token == "" {
		t.Fatalf("token missing from the create response: %v", m)
	}
	if _, has := m["last_used_at"]; has {
		t.Fatalf("last_used_at present (%v), want always absent", m["last_used_at"])
	}
}

// Fails if the list ever includes next_cursor, does not honour ?cursor=, does
// not fix the page at 50 regardless of ?limit, or leaks token on a list item
// (§6).
func TestRobotAccountListFixedPageNoNextCursorHonoursCursor(t *testing.T) {
	s := newClusterFake(t)
	var ids []string
	for i := 0; i < 60; i++ {
		ids = append(ids, s.SeedRobot(testOrg, uuid.NewString(), []string{"push"}))
	}
	sortedIDs := append([]string(nil), ids...)
	sort.Strings(sortedIDs)

	_, b := call(t, s, http.MethodGet, robotsPath+"?limit=1", "") // ?limit is ignored (§6)
	m := decode(t, b)
	if _, has := m["next_cursor"]; has {
		t.Fatalf("list carries next_cursor, want it NEVER present: %v", m)
	}
	items := m["items"].([]any)
	if len(items) != robotPageSize {
		t.Fatalf("page 1 = %d items, want the fixed page size %d regardless of ?limit", len(items), robotPageSize)
	}
	for i, it := range items {
		robot := it.(map[string]any)
		if robot["id"] != sortedIDs[i] {
			t.Fatalf("item %d id = %v, want %v (ORDER BY id)", i, robot["id"], sortedIDs[i])
		}
		if _, has := robot["token"]; has {
			t.Fatalf("list item leaked token: %v", robot)
		}
		if robot["harbor_username"] == nil || robot["harbor_username"] == "" {
			t.Fatalf("list item missing harbor_username: %v", robot)
		}
	}
	last := sortedIDs[robotPageSize-1]

	_, b2 := call(t, s, http.MethodGet, robotsPath+"?cursor="+last, "")
	m2 := decode(t, b2)
	if _, has := m2["next_cursor"]; has {
		t.Fatalf("page 2 carries next_cursor, want it NEVER present: %v", m2)
	}
	items2 := m2["items"].([]any)
	if len(items2) != len(ids)-robotPageSize {
		t.Fatalf("page 2 = %d items, want %d", len(items2), len(ids)-robotPageSize)
	}
	for i, it := range items2 {
		robot := it.(map[string]any)
		if robot["id"] != sortedIDs[robotPageSize+i] {
			t.Fatalf("page 2 item %d id = %v, want %v", i, robot["id"], sortedIDs[robotPageSize+i])
		}
	}
}

// Fails if a malformed id does not answer the JSON store_error envelope
// mentioning "lookup robot account" (mirroring pgx's uuid cast failure -
// never text/plain, §7 n-4) or a well-formed-but-unknown id is anything but
// 404 (§7).
func TestRobotAccountDeleteMalformedVsUnknownID(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodDelete, robotsPath+"/not-a-uuid", "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("malformed id = %d %s, want 500", resp.StatusCode, b)
	}
	m := decode(t, b)
	if errCode(m) != "store_error" {
		t.Fatalf("malformed id error code = %v, want store_error (JSON, never text/plain - §7 n-4): %v", errCode(m), m)
	}
	if msg, _ := m["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "lookup robot account") {
		t.Fatalf("malformed id message = %q, want it to contain \"lookup robot account\"", msg)
	}

	resp, b = call(t, s, http.MethodDelete, robotsPath+"/"+uuid.NewString(), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("well-formed unknown id = %d %s, want 404", resp.StatusCode, b)
	}
}

// Fails if delete is not 204, or a second delete of the same id is anything
// but 404 (a hard delete, §7).
func TestRobotAccountDeleteThenSecondIs404(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, robotsPath, `{"name":"ci","permissions":["push"]}`)
	id := decode(t, b)["id"].(string)
	p := robotsPath + "/" + id

	resp, b := call(t, s, http.MethodDelete, p, "")
	if resp.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("delete = %d %q, want 204 empty", resp.StatusCode, b)
	}
	if resp, _ := call(t, s, http.MethodDelete, p, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", resp.StatusCode)
	}
}

// Fails if the sticky override does not repeat across MULTIPLE delete calls
// (a one-shot override would only fail the first, letting a "retry" through)
// or if the row is removed despite the injected failure - Harbor-first means
// the row survives (§7).
func TestStickyRobotDeleteFailureRepeatsAndKeepsRow(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, robotsPath, `{"name":"ci","permissions":["push"]}`)
	id := decode(t, b)["id"].(string)
	s.StickyRobotDeleteFailure(testOrg, id, http.StatusServiceUnavailable, "harbor_unavailable",
		"could not revoke the credential in the registry; not deleted")

	for i := 0; i < 4; i++ { // the transport's 1+3 retries
		resp, b := call(t, s, http.MethodDelete, robotsPath+"/"+id, "")
		m := decode(t, b)
		if resp.StatusCode != http.StatusServiceUnavailable || errCode(m) != "harbor_unavailable" {
			t.Fatalf("attempt %d = %d %v, want 503 harbor_unavailable every time", i, resp.StatusCode, m)
		}
	}
	_, lb := call(t, s, http.MethodGet, robotsPath, "")
	found := false
	for _, it := range decode(t, lb)["items"].([]any) {
		if it.(map[string]any)["id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("robot %s missing after every delete failed - the row must be KEPT (Harbor-first, not fail-forward)", id)
	}
}

// Fails if ClearRobotDeleteFailure does not let a subsequent DELETE succeed
// normally, if it removes the WRONG key (a different id), or if it errors
// when no failure was ever armed for that key (must be a safe no-op).
func TestClearRobotDeleteFailureAllowsSubsequentDeleteToSucceed(t *testing.T) {
	s := newClusterFake(t)

	// A no-op clear for a key with nothing armed must not panic or error.
	s.ClearRobotDeleteFailure(testOrg, uuid.NewString())

	_, b := call(t, s, http.MethodPost, robotsPath, `{"name":"ci","permissions":["push"]}`)
	id := decode(t, b)["id"].(string)
	_, b2 := call(t, s, http.MethodPost, robotsPath, `{"name":"other","permissions":["push"]}`)
	otherID := decode(t, b2)["id"].(string)

	s.StickyRobotDeleteFailure(testOrg, id, http.StatusServiceUnavailable, "harbor_unavailable",
		"could not revoke the credential in the registry; not deleted")
	s.StickyRobotDeleteFailure(testOrg, otherID, http.StatusServiceUnavailable, "harbor_unavailable",
		"could not revoke the credential in the registry; not deleted")

	s.ClearRobotDeleteFailure(testOrg, id)

	if resp, b := call(t, s, http.MethodDelete, robotsPath+"/"+id, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete after clearing = %d %s, want 204 (the failure must be gone)", resp.StatusCode, b)
	}
	// The OTHER id's failure must be untouched by clearing id's.
	if resp, b := call(t, s, http.MethodDelete, robotsPath+"/"+otherID, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("delete of the un-cleared id = %d %s, want 503 (clearing one key must not clear another)", resp.StatusCode, b)
	}
}

// ---------------------------------------------------------------- C11: cleanup policies

const minimalPolicyBody = `{"name":"p","action":"delete","conditions":[{"type":"age","op":"gt","value":"30d"}],"repo_scope":[],"schedule":"nightly"}`

// Fails if an omitted/invalid action, schedule, or a NIL (omitted) repo_scope
// on CREATE is anything but 500 text/plain, mimicking the DB CHECK/NOT-NULL
// violation the explicit INSERT column list produces (§9).
func TestCleanupCreateOmittedRequiredFieldsAre500TextPlain(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"action omitted", `{"name":"p","conditions":[{"type":"age","op":"gt","value":"30d"}],"repo_scope":[],"schedule":"nightly"}`},
		{"action invalid", `{"name":"p","action":"bogus","conditions":[{"type":"age","op":"gt","value":"30d"}],"repo_scope":[],"schedule":"nightly"}`},
		{"schedule omitted", `{"name":"p","action":"delete","conditions":[{"type":"age","op":"gt","value":"30d"}],"repo_scope":[]}`},
		{"schedule invalid", `{"name":"p","action":"delete","conditions":[{"type":"age","op":"gt","value":"30d"}],"repo_scope":[],"schedule":"bogus"}`},
		{"repo_scope omitted", `{"name":"p","action":"delete","conditions":[{"type":"age","op":"gt","value":"30d"}],"schedule":"nightly"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newClusterFake(t)
			resp, b := call(t, s, http.MethodPost, policiesPath, c.body)
			if resp.StatusCode != http.StatusInternalServerError {
				t.Fatalf("status = %d %s, want 500", resp.StatusCode, b)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Fatalf("Content-Type = %q, want text/plain", ct)
			}
		})
	}
}

// Fails if an omitted `conditions` on create errors instead of storing JSON
// null with no error (the ONE field allowed to go missing, §9).
func TestCleanupCreateOmittedConditionsStoresNullNoError(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodPost, policiesPath, `{"name":"p","action":"delete","repo_scope":[],"schedule":"nightly"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d %s, want 201 (conditions may be omitted)", resp.StatusCode, b)
	}
	m := decode(t, b)
	if v, has := m["conditions"]; !has || v != nil {
		t.Fatalf("conditions = %v (present=%v), want JSON null", v, has)
	}
}

// Fails if an empty name on CREATE is anything but 400 text/plain "name is
// required" (§9).
func TestCleanupCreateEmptyNameIs400(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodPost, policiesPath, `{"name":"","action":"delete","conditions":[],"repo_scope":[],"schedule":"nightly"}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "name is required") {
		t.Fatalf("status/body = %d %q, want 400 \"name is required\"", resp.StatusCode, b)
	}
}

// Fails if the create response carries ANY last_run_* key - a brand new
// policy has never run, and the store query doesn't select them at all (§9).
func TestCleanupCreateResponseOmitsLastRunFields(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, policiesPath, minimalPolicyBody)
	m := decode(t, b)
	for _, k := range []string{"last_run_at", "last_run_deleted", "last_run_freed_bytes", "last_run_message"} {
		if _, has := m[k]; has {
			t.Fatalf("create response carries %s = %v, want it absent", k, m[k])
		}
	}
}

// Fails if the list never shows last_run_at/_deleted/_freed_bytes after a run
// (SetLastRun), or if it EVER shows last_run_message, which no route returns
// (§10).
func TestCleanupListShowsLastRunFieldsExceptMessage(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, policiesPath, minimalPolicyBody)
	id := decode(t, b)["id"].(string)

	_, lb := call(t, s, http.MethodGet, policiesPath, "")
	items := decode(t, lb)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list = %v, want one policy", items)
	}
	fresh := items[0].(map[string]any)
	for _, k := range []string{"last_run_at", "last_run_deleted", "last_run_freed_bytes", "last_run_message"} {
		if _, has := fresh[k]; has {
			t.Fatalf("a never-run policy carries %s, want absent", k)
		}
	}

	if !s.SetLastRun(testOrg, id, 3, 1024, "some artifacts skipped") {
		t.Fatalf("SetLastRun reported the policy missing")
	}
	_, lb2 := call(t, s, http.MethodGet, policiesPath, "")
	ran := decode(t, lb2)["items"].([]any)[0].(map[string]any)
	if ran["last_run_deleted"] != float64(3) || ran["last_run_freed_bytes"] != float64(1024) {
		t.Fatalf("ran policy = %v, want last_run_deleted=3 last_run_freed_bytes=1024", ran)
	}
	if ran["last_run_at"] == nil || ran["last_run_at"] == "" {
		t.Fatalf("ran policy last_run_at missing: %v", ran)
	}
	if _, has := ran["last_run_message"]; has {
		t.Fatalf("ran policy carries last_run_message = %v, want it NEVER returned by any route", ran["last_run_message"])
	}
}

// Fails if PUT does not fully replace name/action/conditions/repo_scope/
// schedule, if it does not merge (leave unchanged) an omitted `enabled`, or
// if its response carries last_run_* (§11).
func TestCleanupPutFullReplaceAndEnabledMerge(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, policiesPath, minimalPolicyBody) // enabled defaults true
	id := decode(t, b)["id"].(string)
	if !s.SetLastRun(testOrg, id, 9, 999, "stale") {
		t.Fatal("seed SetLastRun failed")
	}

	putBody := `{"name":"p2","action":"protect","conditions":[{"type":"tag_count","op":"gt","value":"5"}],"repo_scope":["foo/*"],"schedule":"weekly"}` // enabled omitted
	resp, pb := call(t, s, http.MethodPut, policiesPath+"/"+id, putBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", resp.StatusCode, pb)
	}
	m := decode(t, pb)
	if m["name"] != "p2" || m["action"] != "protect" || m["schedule"] != "weekly" {
		t.Fatalf("PUT result = %v, want the full replacement", m)
	}
	if m["enabled"] != true {
		t.Fatalf("enabled = %v, want unchanged true (omitted enabled must merge)", m["enabled"])
	}
	for _, k := range []string{"last_run_at", "last_run_deleted", "last_run_freed_bytes", "last_run_message"} {
		if _, has := m[k]; has {
			t.Fatalf("PUT response carries %s, want absent (§11)", k)
		}
	}

	// enabled=false is honoured when explicitly sent.
	resp2, pb2 := call(t, s, http.MethodPut, policiesPath+"/"+id, putBody[:len(putBody)-1]+`,"enabled":false}`)
	if resp2.StatusCode != http.StatusOK || decode(t, pb2)["enabled"] != false {
		t.Fatalf("PUT enabled=false = %d %s, want enabled:false in the result", resp2.StatusCode, pb2)
	}
}

// Fails if omitting `conditions` on PUT errors instead of wiping to JSON null
// with no error (§11).
func TestCleanupPutOmittedConditionsWipesToNullNoError(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, policiesPath, minimalPolicyBody)
	id := decode(t, b)["id"].(string)

	resp, pb := call(t, s, http.MethodPut, policiesPath+"/"+id, `{"name":"p","action":"delete","repo_scope":[],"schedule":"nightly"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT omitted conditions = %d %s, want 200", resp.StatusCode, pb)
	}
	if v, has := decode(t, pb)["conditions"]; !has || v != nil {
		t.Fatalf("conditions = %v (present=%v), want wiped to JSON null", v, has)
	}
}

// Fails if PUT to an unknown/foreign id is anything but 500 text/plain with
// the exact measured message - never 404 (§11).
func TestCleanupPutUnknownIDIs500ExactMessage(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodPut, policiesPath+"/"+uuid.NewString(), minimalPolicyBody)
	if resp.StatusCode != http.StatusInternalServerError || strings.TrimSpace(string(b)) != "update cleanup policy: no rows in result set" {
		t.Fatalf("PUT unknown id = %d %q, want 500 \"update cleanup policy: no rows in result set\"", resp.StatusCode, b)
	}
}

// Fails if PUT to an unknown id validates the body BEFORE looking it up: the
// real API's UPDATE matches zero rows first, so an invalid body against an
// unknown id still answers "no rows in result set", never a CHECK-constraint
// message (§11 - "a CHECK/NOT NULL cannot fire when zero rows match", n-5).
func TestCleanupPutUnknownIDIgnoresInvalidBody(t *testing.T) {
	s := newClusterFake(t)
	// action/schedule/repo_scope all omitted - would 500 with the CHECK-
	// constraint message instead if validated before the lookup.
	resp, b := call(t, s, http.MethodPut, policiesPath+"/"+uuid.NewString(), `{"name":"p"}`)
	if resp.StatusCode != http.StatusInternalServerError || strings.TrimSpace(string(b)) != "update cleanup policy: no rows in result set" {
		t.Fatalf("PUT invalid body to unknown id = %d %q, want 500 \"update cleanup policy: no rows in result set\" regardless of body", resp.StatusCode, b)
	}
}

// Fails if a PUT issued after SetLastRun moves last_run_at: the real PUT's
// query does not select or write last_run_* - only a run does (§11/§12,
// m-2). Before the fix, last_run_at was rendered from updatedSeq, which a
// PUT also bumps.
func TestCleanupPutAfterRunKeepsLastRunAt(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, policiesPath, minimalPolicyBody)
	id := decode(t, b)["id"].(string)
	if !s.SetLastRun(testOrg, id, 3, 1024, "some artifacts skipped") {
		t.Fatal("SetLastRun reported the policy missing")
	}
	_, lb := call(t, s, http.MethodGet, policiesPath, "")
	before, _ := decode(t, lb)["items"].([]any)[0].(map[string]any)["last_run_at"].(string)
	if before == "" {
		t.Fatal("last_run_at empty right after SetLastRun")
	}

	resp, pb := call(t, s, http.MethodPut, policiesPath+"/"+id, minimalPolicyBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %s, want 200", resp.StatusCode, pb)
	}

	_, lb2 := call(t, s, http.MethodGet, policiesPath, "")
	after, _ := decode(t, lb2)["items"].([]any)[0].(map[string]any)["last_run_at"].(string)
	if after != before {
		t.Fatalf("last_run_at moved after a PUT (%s -> %s), want unchanged (only a run moves it, m-2)", before, after)
	}
}

// Fails if DELETE of an unknown/foreign id is anything but 500 text/plain
// with the exact measured message - never 404 (§11).
func TestCleanupDeleteUnknownIDIs500ExactMessage(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodDelete, policiesPath+"/"+uuid.NewString(), "")
	if resp.StatusCode != http.StatusInternalServerError || strings.TrimSpace(string(b)) != "cleanup policy not found" {
		t.Fatalf("DELETE unknown id = %d %q, want 500 \"cleanup policy not found\"", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
}

// Fails if a real DELETE does not remove the policy (checked via a
// subsequent list), or is not 204.
func TestCleanupDeleteRemovesThePolicy(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, policiesPath, minimalPolicyBody)
	id := decode(t, b)["id"].(string)

	resp, db := call(t, s, http.MethodDelete, policiesPath+"/"+id, "")
	if resp.StatusCode != http.StatusNoContent || len(db) != 0 {
		t.Fatalf("delete = %d %q, want 204 empty", resp.StatusCode, db)
	}
	_, lb := call(t, s, http.MethodGet, policiesPath, "")
	if items := decode(t, lb)["items"].([]any); len(items) != 0 {
		t.Fatalf("policies after delete = %v, want none", items)
	}
}

// Fails if updated_at does not move on a successful PUT (§11).
func TestCleanupUpdatedAtMovesOnPut(t *testing.T) {
	s := newClusterFake(t)
	_, b := call(t, s, http.MethodPost, policiesPath, minimalPolicyBody)
	created := decode(t, b)
	id, before := created["id"].(string), created["updated_at"].(string)

	_, pb := call(t, s, http.MethodPut, policiesPath+"/"+id, minimalPolicyBody)
	after := decode(t, pb)["updated_at"].(string)
	if after == before {
		t.Fatalf("updated_at unchanged after PUT (%s), want it to move", after)
	}
}

// Fails if conditions are not stored/returned verbatim across every JSON
// value shape the engine (and other clients) can leave behind: a numeric
// value, a bool value, and the fail-open portal shape with no op/value at
// all (§9 U3).
func TestCleanupConditionsRoundTripEveryValueShape(t *testing.T) {
	s := newClusterFake(t)
	id := s.SeedPolicy(testOrg, "p", "delete", []map[string]any{
		{"type": "tag_count", "op": "gt", "value": float64(10)},
		{"type": "untagged"}, // portal's fail-open shape: no op, no value
		{"type": "tag_name", "op": "eq", "value": "latest"},
	}, []string{}, "nightly", true)

	_, lb := call(t, s, http.MethodGet, policiesPath, "")
	items := decode(t, lb)["items"].([]any)
	var found map[string]any
	for _, it := range items {
		if it.(map[string]any)["id"] == id {
			found = it.(map[string]any)
		}
	}
	if found == nil {
		t.Fatalf("seeded policy %s not found in list", id)
	}
	conds, _ := found["conditions"].([]any)
	if len(conds) != 3 {
		t.Fatalf("conditions = %v, want 3 entries verbatim", conds)
	}
	c0 := conds[0].(map[string]any)
	if c0["value"] != float64(10) {
		t.Fatalf("conditions[0].value = %v (%T), want float64(10) preserved as a NUMBER", c0["value"], c0["value"])
	}
	c1 := conds[1].(map[string]any)
	if _, has := c1["op"]; has {
		t.Fatalf("conditions[1] = %v, want no \"op\" key (the portal's fail-open shape)", c1)
	}
	if _, has := c1["value"]; has {
		t.Fatalf("conditions[1] = %v, want no \"value\" key", c1)
	}
	c2 := conds[2].(map[string]any)
	if c2["value"] != "latest" {
		t.Fatalf("conditions[2].value = %v, want the string \"latest\" preserved", c2["value"])
	}
}
