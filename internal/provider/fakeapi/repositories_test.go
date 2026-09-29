package fakeapi

// The fake's own tests for repositories.go (SDD task-14-measured-contract.md
// §1-§2). testOrg/newClusterFake/call/decode/errCode are shared helpers from
// clusters_test.go/nodepools_test.go (same package).
//
// Defect classes each test below is written to catch (per task-14 brief's
// "mutation-tested against at least 5 defect classes"):
//   D1 wrong status code (502 vs 200, 404 vs 200) for a never-existed
//      project / missing repository
//   D2 next_cursor wrong: absent when it must be present, present when it
//      must be "", or off-by-one at the page boundary
//   D3 optional-field omission inverted (last_push/pushed_at/pulled_at/
//      vulnerabilities rendered as null/zero instead of omitted, or vice
//      versa)
//   D4 cursor/limit not honoured (SetHarborPageCap ignored, or `cursor`
//      not advancing the page)
//   D5 tag cardinality/identity wrong (untagged name, digest repetition,
//      item count vs artifact count conflated)
//   D6 routing wrong for a nested repository name (a literal "/" instead of
//      the escaped "%2F" breaking the route, or double-unescaping)
//   D7 repositories/tags state coupled to registryStore's enable/disable
//      (must NOT be - a disable-without-purge must still list)

import (
	"net/http"
	"testing"
	"time"
)

const repositoriesPath = "/organizations/" + testOrg + "/registry/repositories"

// D1: a project nobody has ever created/seeded answers 502 harbor_error, not
// 200 empty and not 404.
func TestRepositoriesListNeverExistedIs502(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodGet, repositoriesPath, "")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("GET repositories (never existed) = %d %s, want 502", resp.StatusCode, b)
	}
	m := decode(t, b)
	if errCode(m) != "harbor_error" {
		t.Fatalf("error code = %q, want harbor_error", errCode(m))
	}
}

// D7: seeding a repository, then disabling the registry (registry.go's own
// route, independent store) must NOT remove it from the repositories list -
// "disabled without purge still lists" falls out of the two stores being
// decoupled.
func TestRepositoriesListSurvivesRegistryDisable(t *testing.T) {
	s := newClusterFake(t)
	s.SeedRepository(testOrg, "app", 3, 1024, nil)
	enableRegistryFor(t, s) // creates the registryStore row (org-acme robot etc.)
	s.Disable(testOrg)      // registry.go: deletes the registryStore row + robots
	resp, b := call(t, s, http.MethodGet, repositoriesPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET repositories after disable = %d %s, want 200 (repos survive a disable without purge)", resp.StatusCode, b)
	}
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items after disable = %v, want the one seeded repository still listed", m["items"])
	}
}

// D1/D3: a project that exists but holds nothing answers 200 with an empty
// list and next_cursor "" - not an error, and next_cursor is present (not
// omitted).
func TestRepositoriesListEmptyProjectIsEmptyListNotError(t *testing.T) {
	s := newClusterFake(t)
	s.MarkRegistryProjectExists(testOrg)
	resp, b := call(t, s, http.MethodGet, repositoriesPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET repositories (empty project) = %d %s, want 200", resp.StatusCode, b)
	}
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("items = %v, want empty", m["items"])
	}
	nc, present := m["next_cursor"]
	if !present || nc != "" {
		t.Fatalf("next_cursor = %v (present=%v), want present and \"\"", nc, present)
	}
}

// D3: last_push is OMITTED (not null, not "") when the seed's pointer is
// nil, and rendered as an RFC3339 string when set. size_bytes 0 is a legal
// value on the wire either way (no distinguishing signal - contract §1).
func TestRepositoriesListLastPushOmittedWhenNil(t *testing.T) {
	s := newClusterFake(t)
	push := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	s.SeedRepository(testOrg, "no-push", 0, 0, nil)
	s.SeedRepository(testOrg, "with-push", 5, 2048, &push)
	resp, b := call(t, s, http.MethodGet, repositoriesPath, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET repositories = %d %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want 2", items)
	}
	byName := map[string]map[string]any{}
	for _, it := range items {
		r := it.(map[string]any)
		byName[r["name"].(string)] = r
	}
	if _, has := byName["no-push"]["last_push"]; has {
		t.Fatalf("no-push repository carries last_push = %v, want the key OMITTED", byName["no-push"]["last_push"])
	}
	if got := byName["with-push"]["last_push"]; got != push.Format(time.RFC3339) {
		t.Fatalf("with-push last_push = %v, want %q", got, push.Format(time.RFC3339))
	}
	if _, has := byName["no-push"]["vulnerabilities"]; has {
		t.Fatalf("repository carries vulnerabilities = %v, want the key ALWAYS omitted (§1: never populated)", byName["no-push"]["vulnerabilities"])
	}
}

// D2/D4: SetHarborPageCap forces a small page regardless of the requested
// limit, so 3 seeded repositories with a cap of 2 and limit=100 page as
// [2,1] with next_cursor "2" then "".
func TestRepositoriesListPagesByCursorAndStopsOnEmptyNextCursor(t *testing.T) {
	s := newClusterFake(t)
	s.SeedRepository(testOrg, "a", 1, 10, nil)
	s.SeedRepository(testOrg, "b", 1, 10, nil)
	s.SeedRepository(testOrg, "c", 1, 10, nil)
	s.SetHarborPageCap(testOrg, 2)

	resp1, b1 := call(t, s, http.MethodGet, repositoriesPath+"?limit=100", "")
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("page1 = %d %s", resp1.StatusCode, b1)
	}
	m1 := decode(t, b1)
	items1, _ := m1["items"].([]any)
	if len(items1) != 2 {
		t.Fatalf("page1 items = %v, want 2 (capped)", items1)
	}
	if m1["next_cursor"] != "2" {
		t.Fatalf("page1 next_cursor = %v, want \"2\"", m1["next_cursor"])
	}

	resp2, b2 := call(t, s, http.MethodGet, repositoriesPath+"?limit=100&cursor=2", "")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("page2 = %d %s", resp2.StatusCode, b2)
	}
	m2 := decode(t, b2)
	items2, _ := m2["items"].([]any)
	if len(items2) != 1 {
		t.Fatalf("page2 items = %v, want 1", items2)
	}
	if m2["next_cursor"] != "" {
		t.Fatalf("page2 next_cursor = %v, want \"\" (last page)", m2["next_cursor"])
	}
}

// SetRepeatingHarborCursor (ruling T14-15 support): the forced cursor is
// served as next_cursor even PAST the point where the 1-item seeded list
// would naturally end ("" ), and again identically on the following page -
// the "always answers next_cursor:\"2\"" fake behaviour a provider-level
// test uses to prove the pager fails loud on a repeated cursor.
func TestRepositoriesListRepeatingCursorIsForcedPastNaturalEnd(t *testing.T) {
	s := newClusterFake(t)
	s.SeedRepository(testOrg, "app", 1, 10, nil)
	s.SetRepeatingHarborCursor(testOrg, "2")

	_, b1 := call(t, s, http.MethodGet, repositoriesPath, "")
	m1 := decode(t, b1)
	if m1["next_cursor"] != "2" {
		t.Fatalf("page1 next_cursor = %v, want the forced \"2\" even though a 1-item list would naturally end here", m1["next_cursor"])
	}

	_, b2 := call(t, s, http.MethodGet, repositoriesPath+"?cursor=2", "")
	m2 := decode(t, b2)
	if m2["next_cursor"] != "2" {
		t.Fatalf("page2 next_cursor = %v, want the forced \"2\" again (repeating, not \"\")", m2["next_cursor"])
	}
}

// SetOmitNextCursor (coder's suggested pin 2, task-14-report.md "Fix round
// 2"): once armed, the repositories list response drops the "next_cursor"
// key entirely - not merely sends it as "" - even though the seeded list is
// a single item and would naturally end there anyway.
func TestRepositoriesListOmitNextCursorDropsTheKey(t *testing.T) {
	s := newClusterFake(t)
	s.SeedRepository(testOrg, "solo", 1, 10, nil)
	s.SetOmitNextCursor(testOrg, true)

	_, b := call(t, s, http.MethodGet, repositoriesPath, "")
	m := decode(t, b)
	if _, ok := m["next_cursor"]; ok {
		t.Fatalf("response %v carries a next_cursor key, want it entirely absent", m)
	}
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want exactly 1", m["items"])
	}
}

// D5: tags_count is honestly the ARTIFACT count (whatever the seed says),
// distinct from an item count - not re-derived from anything else.
func TestRepositoriesListTagsCountIsArtifactCountVerbatim(t *testing.T) {
	s := newClusterFake(t)
	s.SeedRepository(testOrg, "app", 7, 555, nil)
	_, b := call(t, s, http.MethodGet, repositoriesPath, "")
	m := decode(t, b)
	items, _ := m["items"].([]any)
	r := items[0].(map[string]any)
	if n, ok := r["tags_count"].(float64); !ok || int(n) != 7 {
		t.Fatalf("tags_count = %v, want 7", r["tags_count"])
	}
}

// ---------------------------------------------------------------- tags

func tagsPath(repoName string) string {
	return repositoriesPath + "/" + repoName + "/tags"
}

// D1: a missing repository (project exists, repo does not) is 404, matching
// a never-existed project's 404 too (§2 - the two are indistinguishable on
// the wire).
func TestRepositoryTagsListMissingRepoIs404(t *testing.T) {
	s := newClusterFake(t)
	s.MarkRegistryProjectExists(testOrg)
	resp, b := call(t, s, http.MethodGet, tagsPath("nope"), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET tags (missing repo) = %d %s, want 404", resp.StatusCode, b)
	}
}

// D1: a repository under a project that was never created is ALSO 404 (not
// 502, unlike the repositories-list route).
func TestRepositoryTagsListNeverExistedProjectIs404(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodGet, tagsPath("app"), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET tags (never-existed project) = %d %s, want 404", resp.StatusCode, b)
	}
}

// D3/D5: an untagged artifact renders name:"", pushed_at/pulled_at are
// omitted when nil, vulnerabilities is omitted when nil (not scanned) and
// rendered with all five keys (low/unknown included, unlike CG's
// VulnSummary) when set.
func TestRepositoryTagsListUntaggedAndVulnerabilitiesShapes(t *testing.T) {
	s := newClusterFake(t)
	pushed := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	s.SeedTag(testOrg, "app", "v1", "sha256:aaa", 100, &pushed, nil, false, &Vulnerabilities{Critical: 1, High: 2, Medium: 3, Low: 4, Unknown: 5})
	s.SeedTag(testOrg, "app", "", "sha256:bbb", 200, nil, nil, true, nil)
	resp, b := call(t, s, http.MethodGet, tagsPath("app"), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET tags = %d %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want 2", items)
	}
	var scanned, untagged map[string]any
	for _, it := range items {
		tag := it.(map[string]any)
		if tag["digest"] == "sha256:aaa" {
			scanned = tag
		} else {
			untagged = tag
		}
	}
	if scanned == nil || untagged == nil {
		t.Fatalf("items = %v, want one of each digest", items)
	}
	if untagged["name"] != "" || untagged["untagged"] != true {
		t.Fatalf("untagged item = %v, want name:\"\" untagged:true", untagged)
	}
	if _, has := untagged["pushed_at"]; has {
		t.Fatalf("untagged pushed_at = %v, want OMITTED", untagged["pushed_at"])
	}
	if _, has := untagged["vulnerabilities"]; has {
		t.Fatalf("untagged vulnerabilities = %v, want OMITTED (not scanned)", untagged["vulnerabilities"])
	}
	vuln, ok := scanned["vulnerabilities"].(map[string]any)
	if !ok {
		t.Fatalf("scanned vulnerabilities = %v, want an object", scanned["vulnerabilities"])
	}
	for k, want := range map[string]float64{"critical": 1, "high": 2, "medium": 3, "low": 4, "unknown": 5} {
		if got, _ := vuln[k].(float64); got != want {
			t.Fatalf("vulnerabilities[%q] = %v, want %v", k, vuln[k], want)
		}
	}
	if got := scanned["pushed_at"]; got != pushed.Format(time.RFC3339) {
		t.Fatalf("scanned pushed_at = %v, want %q", got, pushed.Format(time.RFC3339))
	}
	if _, has := scanned["pulled_at"]; has {
		t.Fatalf("scanned pulled_at = %v, want OMITTED (nil pointer)", scanned["pulled_at"])
	}
}

// D6: a nested repository name round-trips through the fake's routing, which
// matches on r.URL.EscapedPath() (raw, %2F preserved) rather than the
// net/http-decoded Path every other route in this package uses - the exact
// hazard contract §2/U4 names. Also confirms the recorded Request carries the
// STILL-ESCAPED form in RawPath, which a provider-level test (TG2) asserts
// on.
func TestRepositoryTagsListNestedRepoNameRoutesOnEscapedPath(t *testing.T) {
	s := newClusterFake(t)
	s.SeedTag(testOrg, "team/app", "v1", "sha256:ccc", 10, nil, nil, false, nil)
	resp, b := call(t, s, http.MethodGet, repositoriesPath+"/team%2Fapp/tags", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET tags of a nested repo name = %d %s, want 200", resp.StatusCode, b)
	}
	m := decode(t, b)
	items, _ := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want the one tag of team/app", items)
	}
	reqs := s.Requests()
	last := reqs[len(reqs)-1]
	if last.RawPath == "" || !containsEscapedSlash(last.RawPath) {
		t.Fatalf("recorded RawPath = %q, want it to still carry the escaped %%2F (not a decoded /)", last.RawPath)
	}
}

func containsEscapedSlash(s string) bool {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && s[i+1] == '2' && (s[i+2] == 'F' || s[i+2] == 'f') {
			return true
		}
	}
	return false
}
