package fakeapi

// Registry repositories/tags routes of the fake, reproducing what aether-api
// DOES (measured at aether-api 8e9045a; SDD
// task-14-measured-contract.md §1-§2), not what its OpenAPI spec says.
//
//	GET  /organizations/{org}/registry/repositories[?cursor=&limit=]                      200 {items[],next_cursor} | 502
//	GET  /organizations/{org}/registry/repositories/{repoName}/tags[?cursor=&limit=]       200 {items[],next_cursor} | 404 | 502
//
// Behaviours that matter and that the fake copies (contract section numbers
// below):
//   - BOTH routes are a live call to a per-org "Harbor project" that this
//     fake models independently of registryStore (registry.go): a project
//     exists once SeedRepository/SeedTag has ever been called for the org
//     (MarkRegistryProjectExists for an enabled-but-empty project), and NEVER
//     stops existing again - disabling aether_registry (even with purge)
//     does not touch this store, because the real handler never consults the
//     registries row either (§1 "the handler never consults the registries
//     row") and nothing here actually purges an image (registry.go's own
//     comment on PurgeCount). This is what makes "disabled without purge
//     still lists" fall out for free: seed the repository, then disable the
//     registry through registry.go/rawCall - the repository list is
//     unaffected;
//   - a project that has never existed answers 502 harbor_error "list repos"
//     for repositories (§1) but 404 not_found "Resource not found" for tags
//     (§2 - "Missing repo OR never-enabled registry -> 404... the two cases
//     are indistinguishable") - the two routes deliberately do NOT share one
//     "enabled" gate;
//   - `next_cursor` is ALWAYS present in the body, as a Harbor PAGE NUMBER
//     string ("2", "3", ...), "" on the last page - never a UUID, never
//     omitted (§1/§2);
//   - `?cursor=` and `?limit=` are honoured. `limit<=0` or unparseable falls
//     back to 50; a positive limit is NOT capped by the route itself, but
//     SetHarborPageCap lets a test force Harbor's own (unmeasured, U2) cap so
//     a small seed can still exercise multi-page paging deterministically
//     without needing >100 rows;
//   - repository `size_bytes: 0`, `vulnerabilities` (never populated) and
//     `last_push` (omitted when nil) all mirror §1's "could not look"/
//     "silent zero" table;
//   - tag items are one per (artifact x tag): `name` is "" for an untagged
//     artifact, the same `digest` can repeat, `pushed_at`/`pulled_at` are
//     omitted (not null, not zero-time) when the seed's pointer is nil, and
//     `vulnerabilities` is omitted unless the seed supplies one - "not
//     scanned" and "scanned, zero findings" are different SeedTag calls
//     (§2);
//   - the tags route matches on the RAW escaped path
//     (Server.serve's rawPath, r.URL.EscapedPath()), never the
//     already-percent-decoded Path used by every other route in this
//     package: a nested repository name arrives as `team%2Fapp` on the wire
//     (the generated client's runtime.StyleParamWithOptions path-escapes it
//     once, contract §2 U4), and net/http's automatic decoding of
//     r.URL.Path would otherwise turn that into an extra, un-routable "/"
//     before this handler ever saw it. The looked-up repository name is
//     url.PathUnescape'd exactly once from that raw segment - never twice
//     (Harbor's own accidental double-escape, HR:347, is a detail between
//     aether-api and Harbor that this fake does not model or need to).

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

var (
	repositoriesPathRE   = regexp.MustCompile(`^/organizations/([^/]+)/registry/repositories$`)
	repositoryTagsPathRE = regexp.MustCompile(`^/organizations/([^/]+)/registry/repositories/(.+)/tags$`)
)

// Vulnerabilities is the {critical,high,medium,low,unknown} scan summary a
// test attaches to a seeded tag - see SeedTag. A nil *Vulnerabilities means
// "not successfully scanned" (contract §2), which is NOT the same as a
// Vulnerabilities{} zero value ("scanned, nothing found").
type Vulnerabilities struct {
	Critical, High, Medium, Low, Unknown int
}

type fakeRepository struct {
	name          string
	artifactCount int
	sizeBytes     int64
	lastPush      *time.Time // nil = omitted (§1)
}

type fakeTag struct {
	name      string // "" = untagged (§2)
	digest    string
	sizeBytes int64
	pushedAt  *time.Time // nil = omitted, never zero-time (§2)
	pulledAt  *time.Time
	untagged  bool
	vuln      *Vulnerabilities // nil = not successfully scanned (§2)
}

// repositoryStore is embedded by value in Server; its zero value is usable.
type repositoryStore struct {
	mu sync.Mutex

	everExisted map[string]bool                  // by org: the Harbor project has been created at least once (never reverts)
	repos       map[string][]*fakeRepository     // by org, insertion order
	tags        map[string]map[string][]*fakeTag // by org -> repoName -> tags, insertion order

	// pageCap, keyed by org, caps every page this org's routes ever return,
	// REGARDLESS of the caller's requested limit - modelling Harbor's own
	// (unmeasured, U2) page-size ceiling closely enough that a test can force
	// multi-page pagination with a small seed. 0/absent = no cap beyond the
	// caller's own limit (the contract's "no upper cap" default, §1).
	pageCap map[string]int

	// forceCursor, keyed by org, when present makes BOTH the repositories
	// list and the tags list report this value as next_cursor on EVERY page,
	// overriding whatever the normal Harbor-page arithmetic would compute -
	// a fake server contract violation (see SetRepeatingHarborCursor).
	forceCursor map[string]string

	// omitNextCursor, keyed by org, when true drops the "next_cursor" key
	// entirely from the repositories list response (as opposed to sending it
	// present but ""), regardless of pagination state - a future API that
	// stops sending the key on the last page (r1 N-5) instead of sending it
	// empty. See SetOmitNextCursor.
	omitNextCursor map[string]bool
}

func (rp *repositoryStore) init() {
	if rp.everExisted == nil {
		rp.everExisted = map[string]bool{}
	}
	if rp.repos == nil {
		rp.repos = map[string][]*fakeRepository{}
	}
	if rp.tags == nil {
		rp.tags = map[string]map[string][]*fakeTag{}
	}
	if rp.pageCap == nil {
		rp.pageCap = map[string]int{}
	}
	if rp.forceCursor == nil {
		rp.forceCursor = map[string]string{}
	}
	if rp.omitNextCursor == nil {
		rp.omitNextCursor = map[string]bool{}
	}
}

// ---------------------------------------------------------------- knobs

// MarkRegistryProjectExists records that org's Harbor project exists (so
// GET repositories/tags stop answering "never enabled"), without seeding any
// repository - the shape of a freshly `aether_registry`-enabled, still-empty
// project.
func (s *Server) MarkRegistryProjectExists(org string) {
	rp := &s.repositories
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.init()
	rp.everExisted[org] = true
}

// SeedRepository adds a repository out of band (no request recorded, no
// validation) and implies MarkRegistryProjectExists. lastPush nil renders the
// field omitted (§1); a zero sizeBytes is indistinguishable on the wire from
// a failed size scan, by design (§1).
func (s *Server) SeedRepository(org, name string, artifactCount int, sizeBytes int64, lastPush *time.Time) {
	rp := &s.repositories
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.init()
	rp.everExisted[org] = true
	rp.repos[org] = append(rp.repos[org], &fakeRepository{name: name, artifactCount: artifactCount, sizeBytes: sizeBytes, lastPush: lastPush})
}

// SeedTag adds one (artifact x tag) item under org/repoName out of band, and
// implies MarkRegistryProjectExists for org (a repository cannot exist
// without its project). It does NOT implicitly add repoName to
// SeedRepository's list - a test exercising the tags route alone does not
// need a matching repositories-route seed.
func (s *Server) SeedTag(org, repoName, tagName, digest string, sizeBytes int64, pushedAt, pulledAt *time.Time, untagged bool, vuln *Vulnerabilities) {
	rp := &s.repositories
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.init()
	rp.everExisted[org] = true
	if rp.tags[org] == nil {
		rp.tags[org] = map[string][]*fakeTag{}
	}
	rp.tags[org][repoName] = append(rp.tags[org][repoName], &fakeTag{
		name: tagName, digest: digest, sizeBytes: sizeBytes,
		pushedAt: pushedAt, pulledAt: pulledAt, untagged: untagged, vuln: vuln,
	})
}

// SetHarborPageCap forces every repositories/tags page this org's routes
// serve to hold at most n items, however large a limit the caller requests -
// see repositoryStore.pageCap's doc comment. n<=0 removes the cap.
func (s *Server) SetHarborPageCap(org string, n int) {
	rp := &s.repositories
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.init()
	if n <= 0 {
		delete(rp.pageCap, org)
		return
	}
	rp.pageCap[org] = n
}

// SetRepeatingHarborCursor forces every registry repositories/tags page
// org's routes serve to report cursor as next_cursor forever - INCLUDING
// past the point where the seeded list is exhausted - regardless of what the
// normal Harbor-page arithmetic would compute. This models a broken Harbor
// page contract (the exact fake behaviour SDD task-14-review-r1.md's M-1
// names: "a fake or server that always answers next_cursor:\"2\"") so a
// provider-level test can prove the collectAll pager used by
// aether_registry_repositories and aether_registry_repository_tags (ruling
// T14-15) fails loud on a repeated cursor instead of silently truncating the
// list.
func (s *Server) SetRepeatingHarborCursor(org, cursor string) {
	rp := &s.repositories
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.init()
	rp.forceCursor[org] = cursor
}

// SetOmitNextCursor, when omit is true, makes the registry repositories list
// response for org drop the "next_cursor" key entirely instead of sending it
// present-but-"" on the last page. This is the coder's suggested pin 2
// (task-14-report.md "Fix round 2"): decodeHarborPage (r1 N-5) must read an
// ABSENT key the same as an empty one, since a future API move to the common
// ListResponse (aether-api#311, omitempty) would omit the key rather than
// send it empty. Applies only to the repositories route (the pin names it
// specifically); n<=0/false removes the override.
func (s *Server) SetOmitNextCursor(org string, omit bool) {
	rp := &s.repositories
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.init()
	rp.omitNextCursor[org] = omit
}

// ---------------------------------------------------------------- rendering + paging

func repositoryJSON(r *fakeRepository) map[string]any {
	m := map[string]any{"name": r.name, "tags_count": r.artifactCount, "size_bytes": r.sizeBytes}
	if r.lastPush != nil {
		m["last_push"] = r.lastPush.Format(time.RFC3339)
	}
	// vulnerabilities: NEVER included (§1 - never populated by the handler).
	return m
}

func tagJSON(t *fakeTag) map[string]any {
	m := map[string]any{"name": t.name, "digest": t.digest, "size_bytes": t.sizeBytes, "untagged": t.untagged}
	if t.pushedAt != nil {
		m["pushed_at"] = t.pushedAt.Format(time.RFC3339)
	}
	if t.pulledAt != nil {
		m["pulled_at"] = t.pulledAt.Format(time.RFC3339)
	}
	if t.vuln != nil {
		m["vulnerabilities"] = map[string]any{
			"critical": t.vuln.Critical, "high": t.vuln.High, "medium": t.vuln.Medium,
			"low": t.vuln.Low, "unknown": t.vuln.Unknown,
		}
	}
	return m
}

// harborLimit mirrors atoiDefault(...,50) (RH:389-397): unparseable or <=0
// falls back to 50, any positive value is otherwise accepted.
func harborLimit(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 50
	}
	return n
}

// harborPage renders and pages a Harbor-style listing: page is a 1-based page
// NUMBER (the wire cursor), capped at cap when cap>0. next is "" on the last
// page, else the next page number as a string - both ALWAYS present in the
// caller's response body (§1/§2).
func harborPage[T any](all []T, render func(T) map[string]any, cursor string, limit, pageCap int) (items []map[string]any, next string) {
	if pageCap > 0 && limit > pageCap {
		limit = pageCap
	}
	page := 1
	if cursor != "" {
		if n, err := strconv.Atoi(cursor); err == nil && n > 0 {
			page = n
		}
	}
	start := (page - 1) * limit
	items = []map[string]any{}
	if start >= len(all) {
		return items, ""
	}
	end := min(start+limit, len(all))
	for _, x := range all[start:end] {
		items = append(items, render(x))
	}
	if end < len(all) {
		next = strconv.Itoa(page + 1)
	}
	return items, next
}

// ---------------------------------------------------------------- routing

// routeRepositories serves the two registry repositories/tags routes; it
// reports false when rawPath (r.URL.EscapedPath(), NOT the decoded Path
// every other route in this package matches on - see the file doc comment)
// is not one of them.
func (s *Server) routeRepositories(w http.ResponseWriter, r *http.Request, rawPath string) bool {
	if m := repositoryTagsPathRE.FindStringSubmatch(rawPath); m != nil {
		org, escapedName := m[1], m[2]
		if !s.orgAccess(w, org) {
			return true
		}
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+rawPath)
			return true
		}
		repoName, err := url.PathUnescape(escapedName)
		if err != nil {
			apiError(w, http.StatusBadRequest, "bad_request", "invalid repository name")
			return true
		}
		s.listRepositoryTags(w, r, org, repoName)
		return true
	}
	if repositoriesPathRE.MatchString(rawPath) {
		org := repositoriesPathRE.FindStringSubmatch(rawPath)[1]
		if !s.orgAccess(w, org) {
			return true
		}
		if r.Method != http.MethodGet {
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+rawPath)
			return true
		}
		s.listRepositories(w, r, org)
		return true
	}
	return false
}

// listRepositories is GET .../registry/repositories (§1): 502 for a project
// that has never existed, else a Harbor-paged, always-next_cursor list.
func (s *Server) listRepositories(w http.ResponseWriter, r *http.Request, org string) {
	rp := &s.repositories
	rp.mu.Lock()
	rp.init()
	exists := rp.everExisted[org]
	all := append([]*fakeRepository(nil), rp.repos[org]...)
	pageCap := rp.pageCap[org]
	forced, forcedOK := rp.forceCursor[org]
	omit := rp.omitNextCursor[org]
	rp.mu.Unlock()
	if !exists {
		apiError(w, http.StatusBadGateway, "harbor_error", "list repos")
		return
	}
	limit := harborLimit(r.URL.Query().Get("limit"))
	items, next := harborPage(all, repositoryJSON, r.URL.Query().Get("cursor"), limit, pageCap)
	if forcedOK {
		next = forced
	}
	body := map[string]any{"items": items}
	if !omit {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

// listRepositoryTags is GET .../repositories/{repoName}/tags (§2): 404 for a
// missing repository OR a never-existed project (the two are the same check
// here, matching the real handler's indistinguishability), else a
// Harbor-paged, always-next_cursor list.
func (s *Server) listRepositoryTags(w http.ResponseWriter, r *http.Request, org, repoName string) {
	rp := &s.repositories
	rp.mu.Lock()
	rp.init()
	all, ok := rp.tags[org][repoName]
	allCopy := append([]*fakeTag(nil), all...)
	pageCap := rp.pageCap[org]
	forced, forcedOK := rp.forceCursor[org]
	rp.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "not_found", "Resource not found")
		return
	}
	limit := harborLimit(r.URL.Query().Get("limit"))
	items, next := harborPage(allCopy, tagJSON, r.URL.Query().Get("cursor"), limit, pageCap)
	if forcedOK {
		next = forced
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
