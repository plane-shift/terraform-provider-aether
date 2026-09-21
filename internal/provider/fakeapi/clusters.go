package fakeapi

// Cluster routes of the fake, shaped after the Cluster, CreateClusterRequest,
// UpdateClusterRequest and UpgradeRequest schemas of api/openapi.yaml:
//
//	GET    /organizations/{org}/clusters                 200 {"items":[Cluster…]}
//	POST   /organizations/{org}/clusters                 202 Cluster (status "provisioning")
//	GET    /organizations/{org}/clusters/{id}            200 Cluster | 404
//	PUT    /organizations/{org}/clusters/{id}            200 Cluster | 404 | 422
//	DELETE /organizations/{org}/clusters/{id}            202 {"status":"deleting"} | 404
//	POST   /organizations/{org}/clusters/{id}/upgrade    202 {"cluster":Cluster,…} | 404 | 422
//
// Errors use the API envelope {"error":{"code","message","details":[…]}} with
// the request id ONLY in the X-Request-ID response header.
//
// Knobs that take an id accept "*" meaning "every cluster, including ones
// created later". A specific id wins over "*".

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const anyCluster = "*"

var clusterPathRE = regexp.MustCompile(`^/organizations/([^/]+)/clusters(?:/([^/]+)(/upgrade)?)?$`)

type fakeCluster struct {
	id, orgID, name, region, k8sVersion, observed string
	plan                                          string // "" = absent (plan_slug omitted)
	publicEndpoint                                bool
	seq                                           int // creation order, for ClusterIDs()
	mutations                                     int // bumps updated_at
	reads                                         int // GETs answered since creation / last upgrade / sequence reset
	deleting                                      bool
	deleteGetsLeft                                int
	upgrading                                     bool // a rollout is in flight (see upgrade())
	upgradeReads                                  int  // GETs answered since the upgrade POST
}

type clusterKnobs struct {
	statuses       map[string][]string
	messages       map[string]string
	deleteAfter    map[string]int
	getFailAfterDe map[string]getFail
	nonJSONCreate  bool
	patches        map[string]int // minor ("1.37") -> latest resolved patch
	lagGets        int            // GETs after an upgrade POST that still read the old status/observed
	lagUpgrading   int            // then this many GETs read "upgrading"
}

type getFail struct {
	code    int
	message string
}

// clusterStore is embedded by value in Server; its zero value is usable.
type clusterStore struct {
	mu       sync.Mutex
	clusters map[string]*fakeCluster
	next     int
	knobs    clusterKnobs
}

func (cs *clusterStore) init() {
	if cs.clusters == nil {
		cs.clusters = map[string]*fakeCluster{}
		cs.knobs = clusterKnobs{
			statuses: map[string][]string{}, messages: map[string]string{},
			deleteAfter: map[string]int{}, getFailAfterDe: map[string]getFail{},
			patches: map[string]int{"1.35": 6, "1.36": 4, "1.37": 2}, lagGets: 2, lagUpgrading: 1,
		}
	}
}

func lookup[T any](m map[string]T, id string) (T, bool) {
	if v, ok := m[id]; ok {
		return v, true
	}
	v, ok := m[anyCluster]
	return v, ok
}

// StatusSequence makes successive GETs of the cluster walk through statuses in
// order; the LAST status repeats forever. Reads are counted per cluster, and
// setting a sequence (or POSTing …/upgrade) restarts the walk for the affected
// clusters. With no sequence a cluster reads "running". A cluster being torn
// down reads "deleting" whatever the sequence says. The Terraform harness'
// own refreshes are GETs too and consume the sequence.
func (s *Server) StatusSequence(id string, statuses ...string) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	s.clusters.knobs.statuses[id] = append([]string(nil), statuses...)
	for cid, c := range s.clusters.clusters {
		if id == anyCluster || id == cid {
			c.reads = 0
		}
	}
}

// SetStatusMessage sets the status_message every GET/POST of the cluster
// carries (omitted while empty).
func (s *Server) SetStatusMessage(id, message string) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	s.clusters.knobs.messages[id] = message
}

// DeleteBecomes404After: after DELETE of the cluster, the next n GETs answer
// 200 with status "deleting"; the GET after that answers 404 and the cluster is
// gone. Default (no knob) is n = 0: the first GET after DELETE is already 404.
func (s *Server) DeleteBecomes404After(id string, n int) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	s.clusters.knobs.deleteAfter[id] = n
}

// GetFailsAfterDelete makes every GET of a cluster that is being deleted answer
// the given status code with an API error envelope carrying message (e.g. 403,
// a token rotated mid-delete). code 0 clears the knob.
func (s *Server) GetFailsAfterDelete(id string, code int, message string) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	if code == 0 {
		delete(s.clusters.knobs.getFailAfterDe, id)
		return
	}
	s.clusters.knobs.getFailAfterDe[id] = getFail{code: code, message: message}
}

// RemoveCluster deletes the cluster out-of-band (no DELETE request involved,
// nothing recorded): its next GET answers 404. id "*" removes every cluster.
func (s *Server) RemoveCluster(id string) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	for cid := range s.clusters.clusters {
		if id == anyCluster || id == cid {
			delete(s.clusters.clusters, cid)
		}
	}
}

// CreateRespondsWithNonJSON makes POST …/clusters answer 202 with a text/plain
// body instead of the Cluster JSON. The cluster IS created in the fake.
func (s *Server) CreateRespondsWithNonJSON(on bool) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	s.clusters.knobs.nonJSONCreate = on
}

// ClusterIDs returns the ids of the clusters currently held, oldest first.
func (s *Server) ClusterIDs() []string {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	cs := make([]*fakeCluster, 0, len(s.clusters.clusters))
	for _, c := range s.clusters.clusters {
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].seq < cs[j].seq })
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.id
	}
	return ids
}

// SetLatestPatch sets the latest patch the fake has "resolved" for a minor
// (e.g. "1.37", 5). Bare minors resolve to it at create/upgrade time; an
// explicit patch above it is refused with 422. Defaults: 1.35->6, 1.36->4,
// 1.37->2. A minor not in the table is refused with 422 (unsupported).
func (s *Server) SetLatestPatch(minor string, patch int) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	s.clusters.knobs.patches[minor] = patch
}

// SetStoredVersionAhead puts the cluster in the state a rollout leaves behind
// (started from the portal, or ended `failed`): the API-stored k8s_version
// becomes `stored` while observed_kubernetes_version and status are untouched
// (status still follows StatusSequence). If observed was never reported it is
// pinned to the version the cluster stored before, as a running cluster would
// have reported it. Upgrades are validated against the STORED version.
func (s *Server) SetStoredVersionAhead(id, stored string) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	for cid, c := range s.clusters.clusters {
		if id == anyCluster || id == cid {
			if c.observed == "" {
				c.observed = c.k8sVersion
			}
			c.k8sVersion = stored
			c.mutations++
		}
	}
}

// SetUpgradeLag shapes the asynchronous rollout after POST …/upgrade, counted
// in GETs of that cluster: the first `gets` reads still answer the old status
// and old observed_kubernetes_version (the API's row is unchanged until the
// syncer sees the operator move), the next `upgrading` reads answer status
// "upgrading", and the read after that answers the sequence's status (normally
// running) with observed_kubernetes_version = the resolved target. Defaults
// gets=2, upgrading=1, so the first read that shows the finished upgrade is
// read number gets+upgrading+1 = 4.
func (s *Server) SetUpgradeLag(gets, upgrading int) {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	s.clusters.knobs.lagGets, s.clusters.knobs.lagUpgrading = gets, upgrading
}

var codeByStatus = map[int]string{
	http.StatusBadRequest: "bad_request", http.StatusUnauthorized: "unauthorized", http.StatusForbidden: "forbidden",
	http.StatusNotFound: "not_found", http.StatusConflict: "conflict", http.StatusUnprocessableEntity: "validation_error",
	http.StatusTooManyRequests: "rate_limited", http.StatusInternalServerError: "internal", http.StatusServiceUnavailable: "service_unavailable",
}

func codeFor(status int) string {
	if c, ok := codeByStatus[status]; ok {
		return c
	}
	return "error"
}

// resolve mirrors the API: a bare minor becomes MAJOR.MINOR.<latest patch>; an
// explicit patch must not exceed the minor's latest. Caller holds mu.
func (cs *clusterStore) resolve(v string) (string, error) {
	parts := strings.Split(v, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return "", fmt.Errorf("%q is not MAJOR.MINOR[.PATCH]", v)
	}
	minor := parts[0] + "." + parts[1]
	latest, ok := cs.knobs.patches[minor]
	if !ok {
		return "", fmt.Errorf("Kubernetes %s is not offered", minor)
	}
	if len(parts) == 2 {
		return fmt.Sprintf("%s.%d", minor, latest), nil
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 0 {
		return "", fmt.Errorf("%q has no valid patch", v)
	}
	if n > latest {
		return "", fmt.Errorf("patch %d of %s is not resolved yet (latest %d)", n, minor, latest)
	}
	return v, nil
}

// splitVersion returns major, minor, patch of a resolved MAJOR.MINOR.PATCH.
func splitVersion(v string) (int, int, int, bool) {
	var a, b, c int
	if _, err := fmt.Sscanf(v, "%d.%d.%d", &a, &b, &c); err != nil {
		return 0, 0, 0, false
	}
	return a, b, c, true
}

func apiError(w http.ResponseWriter, code int, errCode, message string, details ...map[string]any) {
	if details == nil {
		details = []map[string]any{}
	}
	// The request id rides ONLY in the response header, never in the body.
	w.Header().Set("X-Request-ID", "req-fake-"+uuid.NewString()[:8])
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": errCode, "message": message, "details": details}})
}

// clusterJSON renders the Cluster schema. Caller holds clusters.mu.
func (cs *clusterStore) clusterJSON(c *fakeCluster, status string) map[string]any {
	m := map[string]any{
		"id": c.id, "org_id": c.orgID, "name": c.name, "region": c.region,
		"k8s_version": c.k8sVersion, "status": status, "public_endpoint": c.publicEndpoint,
		"created_at": "2026-09-17T00:00:00Z",
		"updated_at": time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC).Add(time.Duration(c.mutations) * time.Second).Format(time.RFC3339),
	}
	if msg, ok := lookup(cs.knobs.messages, c.id); ok && msg != "" {
		m["status_message"] = msg
	}
	if c.plan != "" {
		m["plan_slug"] = c.plan
	}
	if c.observed != "" {
		m["observed_kubernetes_version"] = c.observed
	}
	// Provisioned-far-enough fields: stable per cluster so they never look
	// like drift; endpoint only once the operator would report one.
	short := c.id[:8]
	m["capi_cluster_name"] = "capi-" + short
	m["keycloak_realm"] = "aether"
	m["harbor_project"] = "aether-acme"
	m["ceph_pool"] = "pool-" + short
	m["lb_subnet"] = "10.64.0.0/24"
	m["worker_subnet"] = "10.64.1.0/24"
	if status == "running" || status == "upgrading" || status == "degraded" {
		m["endpoint"] = "https://" + short + ".eu-fi.example.test:6443"
	}
	return m
}

func (s *Server) orgKnown(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orgs {
		if o.ID == id {
			return true
		}
	}
	return false
}

// routeClusters serves the cluster routes; it reports false when path is not one.
func (s *Server) routeClusters(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	m := clusterPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	org, id, upgrade := m[1], m[2], m[3] != ""
	if !s.orgKnown(org) {
		apiError(w, http.StatusNotFound, "not_found", "organization not found")
		return true
	}
	cs := &s.clusters
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.init()

	switch {
	case id == "" && r.Method == http.MethodGet:
		items := []map[string]any{}
		ids := make([]*fakeCluster, 0)
		for _, c := range cs.clusters {
			if c.orgID == org {
				ids = append(ids, c)
			}
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].seq < ids[j].seq })
		for _, c := range ids {
			items = append(items, cs.clusterJSON(c, cs.statusOf(c, false)))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case id == "" && r.Method == http.MethodPost:
		cs.create(w, org, body)
	case id != "" && !upgrade:
		c, ok := cs.clusters[id]
		if !ok || c.orgID != org {
			apiError(w, http.StatusNotFound, "not_found", "cluster not found")
			return true
		}
		switch r.Method {
		case http.MethodGet:
			cs.get(w, c)
		case http.MethodPut:
			cs.put(w, c, body)
		case http.MethodDelete:
			c.deleting = true
			c.deleteGetsLeft, _ = lookup(cs.knobs.deleteAfter, c.id)
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "deleting"})
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
	case id != "" && upgrade && r.Method == http.MethodPost:
		c, ok := cs.clusters[id]
		if !ok || c.orgID != org {
			apiError(w, http.StatusNotFound, "not_found", "cluster not found")
			return true
		}
		cs.upgrade(w, c, body)
	default:
		apiError(w, http.StatusNotFound, "not_found", "no route "+r.Method+" "+path)
	}
	return true
}

// statusOf reads the current status; advance=true consumes one step of the
// sequence. Caller holds mu.
func (cs *clusterStore) statusOf(c *fakeCluster, advance bool) string {
	if c.deleting {
		return "deleting"
	}
	seq, ok := lookup(cs.knobs.statuses, c.id)
	if !ok || len(seq) == 0 {
		return "running"
	}
	i := c.reads
	if i > len(seq)-1 {
		i = len(seq) - 1
	}
	if advance {
		c.reads++
	}
	return seq[i]
}

// readStatus is the status one GET reports, advancing the fake's state: the
// sequence walk, an in-flight upgrade's lag, and observed_kubernetes_version
// (unset until the cluster first reads running). Caller holds mu.
func (cs *clusterStore) readStatus(c *fakeCluster) string {
	if c.upgrading {
		c.upgradeReads++
		switch {
		case c.upgradeReads <= cs.knobs.lagGets:
			// still the row as it was before the syncer noticed anything
		case c.upgradeReads <= cs.knobs.lagGets+cs.knobs.lagUpgrading:
			return "upgrading"
		default:
			c.upgrading = false
			c.observed = c.k8sVersion
		}
	}
	status := cs.statusOf(c, true)
	if status == "running" && c.observed == "" {
		c.observed = c.k8sVersion
	}
	return status
}

func (cs *clusterStore) create(w http.ResponseWriter, org string, body []byte) {
	var req struct {
		Name           string  `json:"name"`
		Region         string  `json:"region"`
		K8sVersion     string  `json:"k8s_version"`
		Plan           *string `json:"plan"`
		PublicEndpoint *bool   `json:"public_endpoint"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "malformed JSON: "+err.Error())
		return
	}
	var details []map[string]any
	for field, v := range map[string]string{"name": req.Name, "region": req.Region, "k8s_version": req.K8sVersion} {
		if v == "" {
			details = append(details, map[string]any{"field": field, "code": "required"})
		}
	}
	if details != nil {
		sort.Slice(details, func(i, j int) bool { return details[i]["field"].(string) < details[j]["field"].(string) })
		apiError(w, http.StatusUnprocessableEntity, "validation_error", "request validation failed", details...)
		return
	}
	resolved, err := cs.resolve(req.K8sVersion)
	if err != nil {
		apiError(w, http.StatusUnprocessableEntity, "validation_error", err.Error(),
			map[string]any{"field": "k8s_version", "code": "unavailable"})
		return
	}
	cs.next++
	c := &fakeCluster{
		id: uuid.NewString(), orgID: org, name: req.Name, region: req.Region,
		k8sVersion: resolved, seq: cs.next,
	}
	if req.Plan != nil {
		c.plan = *req.Plan // "" means custom billing: plan_slug stays absent
	}
	if req.PublicEndpoint != nil {
		c.publicEndpoint = *req.PublicEndpoint
	}
	cs.clusters[c.id] = c
	if cs.knobs.nonJSONCreate {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
		return
	}
	// Always "provisioning" on the POST, independent of the GET sequence.
	writeJSON(w, http.StatusAccepted, cs.clusterJSON(c, "provisioning"))
}

func (cs *clusterStore) get(w http.ResponseWriter, c *fakeCluster) {
	if c.deleting {
		if f, ok := lookup(cs.knobs.getFailAfterDe, c.id); ok {
			apiError(w, f.code, codeFor(f.code), f.message)
			return
		}
		if c.deleteGetsLeft <= 0 {
			delete(cs.clusters, c.id)
			apiError(w, http.StatusNotFound, "not_found", "cluster not found")
			return
		}
		c.deleteGetsLeft--
	}
	writeJSON(w, http.StatusOK, cs.clusterJSON(c, cs.readStatus(c)))
}

func (cs *clusterStore) put(w http.ResponseWriter, c *fakeCluster, body []byte) {
	var req struct {
		Name           *string `json:"name"`
		K8sVersion     *string `json:"k8s_version"`
		PublicEndpoint *bool   `json:"public_endpoint"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "malformed JSON: "+err.Error())
		return
	}
	if req.PublicEndpoint != nil && *req.PublicEndpoint != c.publicEndpoint {
		apiError(w, http.StatusUnprocessableEntity, "validation_error", "public_endpoint is immutable",
			map[string]any{"field": "public_endpoint", "code": "immutable"})
		return
	}
	if req.Name != nil {
		c.name = *req.Name
	}
	c.mutations++
	writeJSON(w, http.StatusOK, cs.clusterJSON(c, cs.statusOf(c, false)))
}

func (cs *clusterStore) upgrade(w http.ResponseWriter, c *fakeCluster, body []byte) {
	var req struct {
		TargetVersion string `json:"target_version"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.TargetVersion == "" {
		apiError(w, http.StatusUnprocessableEntity, "validation_error", "target_version is required",
			map[string]any{"field": "target_version", "code": "required"})
		return
	}
	if c.upgrading || cs.statusOf(c, false) != "running" {
		apiError(w, http.StatusConflict, "not_ready", "Cluster must be running to upgrade")
		return
	}
	target, err := cs.resolve(req.TargetVersion)
	if err != nil {
		apiError(w, http.StatusUnprocessableEntity, "validation_error", err.Error(),
			map[string]any{"field": "target_version", "code": "unavailable"})
		return
	}
	// Mirrors aether-api internal/api/handler/upgrade.go validateUpgradeStep,
	// which compares the target with the STORED k8s_version (not observed);
	// codes/messages read from that source: 422 validation_error.
	fMaj, fMin, fPat, ok1 := splitVersion(c.k8sVersion)
	tMaj, tMin, tPat, ok2 := splitVersion(target)
	switch {
	case !ok1 || !ok2:
		apiError(w, http.StatusUnprocessableEntity, "validation_error", "cannot parse "+c.k8sVersion+" or "+target)
		return
	case tMaj != fMaj:
		apiError(w, http.StatusUnprocessableEntity, "validation_error",
			fmt.Sprintf("cross-major upgrades are not supported (current %d.%d.%d → target %d.%d.%d)", fMaj, fMin, fPat, tMaj, tMin, tPat))
		return
	case tMin == fMin && tPat <= fPat, tMin < fMin:
		apiError(w, http.StatusUnprocessableEntity, "validation_error",
			fmt.Sprintf("target %d.%d.%d is not newer than current %d.%d.%d", tMaj, tMin, tPat, fMaj, fMin, fPat))
		return
	case tMin > fMin+1:
		apiError(w, http.StatusUnprocessableEntity, "validation_error",
			fmt.Sprintf("upgrades must be incremental: from %d.%d.%d, next allowed minor is %d.%d (got %d.%d)", fMaj, fMin, fPat, fMaj, fMin+1, tMaj, tMin))
		return
	}
	if c.observed == "" {
		c.observed = c.k8sVersion // running, so the operator has reported it
	}
	// The API answers with the row's k8s_version already the resolved target
	// while status is STILL running and observed STILL the old version; the
	// rollout shows up only in later reads (see SetUpgradeLag).
	c.k8sVersion = target
	c.upgrading, c.upgradeReads = true, 0
	c.mutations++
	writeJSON(w, http.StatusAccepted, map[string]any{
		"cluster": cs.clusterJSON(c, "running"), "workers_patched": 0, "workers_skipped": 0,
	})
}
