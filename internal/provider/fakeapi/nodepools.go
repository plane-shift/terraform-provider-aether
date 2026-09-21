package fakeapi

// Node-pool routes of the fake, reproducing what aether-api DOES (measured at
// aether-api 8e9045a, see the SDD task-7 contract), not what its OpenAPI spec says:
//
//	GET    /organizations/{org}/clusters/{cid}/nodepools        200 {"items":[NodePool…],"next_cursor"?}
//	POST   /organizations/{org}/clusters/{cid}/nodepools        202 NodePool (status "provisioning", current_nodes 0)
//	GET    /organizations/{org}/clusters/{cid}/nodepools/{id}   200 NodePool | 404
//	PUT    /organizations/{org}/clusters/{cid}/nodepools/{id}   200 NodePool (partial COALESCE merge) | 400 | 404 | 409 | 422 | 500
//	DELETE /organizations/{org}/clusters/{cid}/nodepools/{id}   202 {"status":"deleting"} | 404
//
// Behaviours that matter to a Terraform provider and that the fake copies:
//   - the statuses that really occur are provisioning|running|scaling|scaling-down|degraded|deleting
//     (never "ready"/"failed"); "degraded" carries status_message and is not terminal;
//   - a PUT never changes status: the PUT response and the reads right after it
//     still say "running" with the OLD current_nodes (see SetNodePoolScaleLag);
//   - unset budgets read 0, unset labels {} and taints [], a taint's value "",
//     and drain_timeout_seconds/scale_down_* are ABSENT until set (never echoed
//     as platform defaults) and can never be cleared by PUT (omitted == null == keep);
//   - node_labels/node_taints in a PUT body are a 400 "Invalid JSON body";
//   - a desired_nodes-only PUT is validated against the STORED min/max but written
//     with min/max re-pinned to desired (422 on a stored 1/1/1, a silent 3/3/3 on a 1/1/5);
//   - there is no cluster-status precondition and DELETE is never refused; a
//     pool in "deleting" keeps its name (409 on reuse) until a GET sweeps it (404).
//
// Knobs that take an id accept "*" meaning "every node pool, including ones
// created later". A specific id wins over "*".

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

var poolPathRE = regexp.MustCompile(`^/organizations/([^/]+)/clusters/([^/]+)/nodepools(?:/([^/]+))?$`)

var poolNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

var reservedPrefixes = []string{"aether.cloud/", "kubernetes.io/", "k8s.io/", "node-role.kubernetes.io/", "node.kubernetes.io/"}

type poolTaint struct {
	Key    string  `json:"key"`
	Value  *string `json:"value,omitempty"`
	Effect string  `json:"effect"`
}

type fakePool struct {
	id, clusterID, name                    string
	vcpus, memoryGB, diskGB                int
	minNodes, maxNodes, desired, current   int
	minVCPUs, maxVCPUs, minMemGB, maxMemGB int
	labels                                 map[string]string
	taints                                 []poolTaint
	drain, scaleDelay, scaleUtil           *int
	seq                                    int
	mutations                              int  // bumps updated_at (also on reconciler-style status/current changes)
	reads                                  int  // GETs answered; indexes the status sequence
	everRunning                            bool // the first "running" read has happened: current_nodes materialised
	firstReady                             bool // first_ready_at latched
	scaling                                bool // a node-count change is converging (fixed pools only)
	scaleReads                             int  // GETs since the PUT that started it
	scalingUp                              bool
	deleting                               bool
	deleteGetsLeft                         int
}

func (p *fakePool) autoscaling() bool { return p.maxVCPUs > p.minVCPUs || p.maxMemGB > p.minMemGB }

type poolKnobs struct {
	statuses      map[string][]string
	messages      map[string]string
	deleteAfter   map[string]int
	getFailAfterD map[string]getFail
	nonJSONCreate bool
	delete404     bool // DELETE answers 404 (the row is already gone)
	lagGets       int  // GETs after a node-count PUT that still read "running" with the old current_nodes
	lagScaling    int  // then this many GETs read "scaling"/"scaling-down"
}

// poolStore is embedded by value in Server; its zero value is usable.
type poolStore struct {
	mu    sync.Mutex
	pools map[string]*fakePool
	next  int
	knobs poolKnobs
}

func (ps *poolStore) init() {
	if ps.pools == nil {
		ps.pools = map[string]*fakePool{}
		ps.knobs = poolKnobs{
			statuses: map[string][]string{}, messages: map[string]string{},
			deleteAfter: map[string]int{}, getFailAfterD: map[string]getFail{},
			lagGets: 1, lagScaling: 1,
		}
	}
}

// NodePoolStatusSequence makes successive GETs of the pool walk through
// statuses in order; the LAST status repeats forever. Setting a sequence
// restarts the walk for the affected pools. With no sequence a pool reads
// "running" from its first GET. current_nodes is 0 and first_ready_at absent
// until the first read that reports "running", where current_nodes becomes
// desired_nodes. status_message appears only while a read reports "degraded"
// (see SetNodePoolStatusMessage). A pool being torn down reads "deleting"
// whatever the sequence says. The Terraform harness' own refreshes are GETs too
// and consume the sequence.
func (s *Server) NodePoolStatusSequence(id string, statuses ...string) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	s.pools.knobs.statuses[id] = append([]string(nil), statuses...)
	for pid, p := range s.pools.pools {
		if id == anyCluster || id == pid {
			p.reads = 0
		}
	}
}

// SetNodePoolStatusMessage sets the status_message a read carries WHILE it
// reports "degraded" (the API writes it only then; otherwise the key is omitted).
func (s *Server) SetNodePoolStatusMessage(id, message string) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	s.pools.knobs.messages[id] = message
}

// SetNodePoolScaleLag shapes the asynchronous convergence after a PUT that
// changes desired_nodes on a FIXED pool that has already read "running",
// counted in GETs of that pool: the first `gets` reads still answer "running"
// with the old current_nodes (the API's reconciler has not ticked), the next
// `scaling` reads answer "scaling" (desired > current) or "scaling-down", and
// the read after that answers "running" with current_nodes == desired_nodes.
// Defaults gets=1, scaling=1, so the first converged read is read number
// gets+scaling+1 = 3 (about 3 s with the resource's 1 s, 2 s, 4 s backoff).
// Autoscaling pools never scale in the fake: desired_nodes is inert for them.
func (s *Server) SetNodePoolScaleLag(gets, scaling int) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	s.pools.knobs.lagGets, s.pools.knobs.lagScaling = gets, scaling
}

// SetNodePoolCurrentNodes moves current_nodes out-of-band, as the cluster
// autoscaler does for an autoscaling pool (desired_nodes never follows it).
// Nothing is recorded and no scaling walk starts.
func (s *Server) SetNodePoolCurrentNodes(id string, n int) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	for pid, p := range s.pools.pools {
		if id == anyCluster || id == pid {
			p.current, p.everRunning, p.scaling = n, true, false
			p.mutations++
		}
	}
}

// NodePoolDeleteBecomes404After: after DELETE of the pool, the next n GETs answer
// 200 with status "deleting"; the GET after that answers 404 (the sweep retired
// the row). Default is n = 0: the first GET after DELETE is already 404.
func (s *Server) NodePoolDeleteBecomes404After(id string, n int) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	s.pools.knobs.deleteAfter[id] = n
}

// NodePoolGetFailsAfterDelete makes every GET of a pool that is being deleted
// answer the given status code with an API error envelope carrying message.
// code 0 clears the knob.
func (s *Server) NodePoolGetFailsAfterDelete(id string, code int, message string) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	if code == 0 {
		delete(s.pools.knobs.getFailAfterD, id)
		return
	}
	s.pools.knobs.getFailAfterD[id] = getFail{code: code, message: message}
}

// RemoveNodePool deletes the pool out-of-band (no DELETE request, nothing
// recorded): its next GET answers 404. id "*" removes every pool.
func (s *Server) RemoveNodePool(id string) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	for pid := range s.pools.pools {
		if id == anyCluster || id == pid {
			delete(s.pools.pools, pid)
		}
	}
}

// NodePoolCreateRespondsWithNonJSON makes POST …/nodepools answer 202 with a
// text/plain body instead of the NodePool JSON. The pool IS created.
func (s *Server) NodePoolCreateRespondsWithNonJSON(on bool) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	s.pools.knobs.nonJSONCreate = on
}

// NodePoolDeleteRespondsNotFound makes DELETE …/nodepools/{id} answer 404
// not_found and drop the pool, as when the pool was retired between the
// caller's last read and its DELETE. The request is recorded.
func (s *Server) NodePoolDeleteRespondsNotFound(on bool) {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	s.pools.knobs.delete404 = on
}

// NodePoolIDs returns the ids of the pools currently held for a cluster
// (any status, including "deleting"), oldest first.
func (s *Server) NodePoolIDs(clusterID string) []string {
	s.pools.mu.Lock()
	defer s.pools.mu.Unlock()
	s.pools.init()
	return s.pools.sortedIDs(clusterID)
}

func (ps *poolStore) sorted(clusterID string) []*fakePool {
	var out []*fakePool
	for _, p := range ps.pools {
		if p.clusterID == clusterID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

func (ps *poolStore) sortedIDs(clusterID string) []string {
	ids := []string{}
	for _, p := range ps.sorted(clusterID) {
		ids = append(ids, p.id)
	}
	return ids
}

func (s *Server) clusterInOrg(org, id string) bool {
	s.clusters.mu.Lock()
	defer s.clusters.mu.Unlock()
	s.clusters.init()
	c, ok := s.clusters.clusters[id]
	return ok && c.orgID == org
}

func detail(field, code, message string) map[string]any {
	return map[string]any{"field": field, "code": code, "message": message}
}

func validationError(w http.ResponseWriter, details []map[string]any) {
	apiError(w, http.StatusUnprocessableEntity, "validation_error", "Request validation failed", details...)
}

// decodeStrict mirrors the API's decodeJSON: unknown fields are a 400.
func decodeStrict(body []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

func validName(name string) []map[string]any {
	switch {
	case name == "":
		return []map[string]any{detail("name", "required", "Name is required")}
	case len(name) > 40 || !poolNameRE.MatchString(name):
		return []map[string]any{detail("name", "invalid", "Name must be 1-40 lowercase alphanumerics or '-', starting and ending alphanumeric")}
	}
	return nil
}

func inRange(field string, v *int, lo, hi int) []map[string]any {
	if v != nil && (*v < lo || *v > hi) {
		return []map[string]any{detail(field, "invalid", field+" must be between "+itoa(lo)+" and "+itoa(hi))}
	}
	return nil
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func validateKnobs(drain, delay, util *int) []map[string]any {
	var d []map[string]any
	d = append(d, inRange("drain_timeout_seconds", drain, 30, 3600)...)
	d = append(d, inRange("scale_down_delay_seconds", delay, 60, 3600)...)
	d = append(d, inRange("scale_down_utilization_percent", util, 10, 90)...)
	return d
}

func validateSize(vcpus, mem int) []map[string]any {
	var d []map[string]any
	if vcpus < 1 || vcpus > 128 {
		d = append(d, detail("vcpus", "invalid", "vcpus must be between 1 and 128"))
	}
	if mem < 1 || mem > 512 {
		d = append(d, detail("memory_gb", "invalid", "memory_gb must be between 1 and 512"))
	}
	return d
}

func validateBounds(min, des, max int) []map[string]any {
	switch {
	case min < 0:
		return []map[string]any{detail("min_nodes", "invalid", "min_nodes must be >= 0")}
	case min > des || des > max:
		return []map[string]any{detail("desired_nodes", "invalid", "require min_nodes <= desired_nodes <= max_nodes")}
	}
	return nil
}

func validateBudget(vcpus, mem, minV, maxV, minM, maxM int) []map[string]any {
	if minV == 0 && maxV == 0 && minM == 0 && maxM == 0 {
		return nil
	}
	var d []map[string]any
	if minV != 0 && minV < vcpus {
		d = append(d, detail("min_vcpus", "invalid", "min_vcpus must be 0 or at least vcpus"))
	}
	if maxV < vcpus {
		d = append(d, detail("max_vcpus", "invalid", "max_vcpus must be at least vcpus"))
	}
	if minM != 0 && minM < mem {
		d = append(d, detail("min_memory_gb", "invalid", "min_memory_gb must be 0 or at least memory_gb"))
	}
	if maxM < mem {
		d = append(d, detail("max_memory_gb", "invalid", "max_memory_gb must be at least memory_gb"))
	}
	if minV > maxV {
		d = append(d, detail("min_vcpus", "invalid", "min_vcpus must not exceed max_vcpus"))
	}
	if minM > maxM {
		d = append(d, detail("min_memory_gb", "invalid", "min_memory_gb must not exceed max_memory_gb"))
	}
	return d
}

func hasReserved(key string) bool {
	for _, p := range reservedPrefixes {
		if len(key) >= len(p) && key[:len(p)] == p {
			return true
		}
	}
	return false
}

func blocksScheduling(t poolTaint) bool { return t.Effect == "NoSchedule" || t.Effect == "NoExecute" }

// poolJSON renders the NodePool schema. Caller holds pools.mu.
func (ps *poolStore) poolJSON(p *fakePool, status string, current int) map[string]any {
	labels := p.labels
	if labels == nil {
		labels = map[string]string{}
	}
	taints := make([]map[string]any, 0, len(p.taints))
	for _, t := range p.taints {
		v := ""
		if t.Value != nil {
			v = *t.Value
		}
		taints = append(taints, map[string]any{"key": t.Key, "value": v, "effect": t.Effect})
	}
	m := map[string]any{
		"id": p.id, "cluster_id": p.clusterID, "name": p.name,
		"vcpus": p.vcpus, "memory_gb": p.memoryGB, "disk_gb": p.diskGB,
		"min_nodes": p.minNodes, "max_nodes": p.maxNodes, "desired_nodes": p.desired, "current_nodes": current,
		"min_vcpus": p.minVCPUs, "max_vcpus": p.maxVCPUs, "min_memory_gb": p.minMemGB, "max_memory_gb": p.maxMemGB,
		"node_labels": labels, "node_taints": taints, "status": status,
		"created_at": "2026-09-17T00:00:00Z",
		"updated_at": time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC).Add(time.Duration(p.mutations) * time.Second).Format(time.RFC3339),
	}
	if p.drain != nil {
		m["drain_timeout_seconds"] = *p.drain
	}
	if p.scaleDelay != nil {
		m["scale_down_delay_seconds"] = *p.scaleDelay
	}
	if p.scaleUtil != nil {
		m["scale_down_utilization_percent"] = *p.scaleUtil
	}
	if p.firstReady {
		m["first_ready_at"] = "2026-09-17T00:01:00Z"
	}
	if status == "degraded" {
		if msg, ok := lookup(ps.knobs.messages, p.id); ok && msg != "" {
			m["status_message"] = msg
		}
	}
	return m
}

// routeNodePools serves the node-pool routes; it reports false when path is not one.
func (s *Server) routeNodePools(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	m := poolPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	org, cid, id := m[1], m[2], m[3]
	if !s.orgKnown(org) || !s.clusterInOrg(org, cid) {
		apiError(w, http.StatusNotFound, "not_found", "Resource not found")
		return true
	}
	ps := &s.pools
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.init()

	switch {
	case id == "" && r.Method == http.MethodGet:
		ps.list(w, r, cid)
	case id == "" && r.Method == http.MethodPost:
		ps.create(w, cid, body)
	case id != "":
		p, ok := ps.pools[id]
		if !ok || p.clusterID != cid {
			apiError(w, http.StatusNotFound, "not_found", "Resource not found")
			return true
		}
		switch r.Method {
		case http.MethodGet:
			ps.get(w, p)
		case http.MethodPut:
			ps.put(w, p, body)
		case http.MethodDelete:
			if p.deleting || ps.knobs.delete404 {
				if ps.knobs.delete404 {
					delete(ps.pools, p.id)
				}
				apiError(w, http.StatusNotFound, "not_found", "Resource not found")
				return true
			}
			p.deleting = true
			p.deleteGetsLeft, _ = lookup(ps.knobs.deleteAfter, p.id)
			writeJSON(w, http.StatusAccepted, map[string]any{"status": "deleting"})
		default:
			apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		}
	default:
		apiError(w, http.StatusNotFound, "not_found", "no route "+r.Method+" "+path)
	}
	return true
}

func (ps *poolStore) list(w http.ResponseWriter, r *http.Request, cid string) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		var n int
		if err := json.Unmarshal([]byte(v), &n); err == nil && n >= 1 && n <= 100 {
			limit = n
		}
	}
	all := ps.sorted(cid)
	start := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		for i, p := range all {
			if p.id == c {
				start = i + 1
			}
		}
	}
	items := []map[string]any{}
	var last string
	for _, p := range all[start:] {
		if len(items) == limit {
			break
		}
		st, cur := ps.peek(p)
		items = append(items, ps.poolJSON(p, st, cur))
		last = p.id
	}
	out := map[string]any{"items": items}
	if len(items) == limit {
		out["next_cursor"] = last
	}
	writeJSON(w, http.StatusOK, out)
}

// peek is the status a read WOULD report without advancing any walk (used by
// list and by PUT/POST responses, which never move the reconciler).
func (ps *poolStore) peek(p *fakePool) (string, int) {
	if p.deleting {
		return "deleting", p.current
	}
	if !p.everRunning {
		return "provisioning", 0
	}
	return "running", p.current
}

func (ps *poolStore) create(w http.ResponseWriter, cid string, body []byte) {
	var req struct {
		Name                        string            `json:"name"`
		Vcpus                       int               `json:"vcpus"`
		MemoryGB                    int               `json:"memory_gb"`
		DiskGB                      int               `json:"disk_gb"`
		DesiredNodes                *int              `json:"desired_nodes"`
		MinNodes                    *int              `json:"min_nodes"`
		MaxNodes                    *int              `json:"max_nodes"`
		MinVcpus                    int               `json:"min_vcpus"`
		MaxVcpus                    int               `json:"max_vcpus"`
		MinMemoryGB                 int               `json:"min_memory_gb"`
		MaxMemoryGB                 int               `json:"max_memory_gb"`
		DrainTimeoutSeconds         *int              `json:"drain_timeout_seconds"`
		ScaleDownDelaySeconds       *int              `json:"scale_down_delay_seconds"`
		ScaleDownUtilizationPercent *int              `json:"scale_down_utilization_percent"`
		NodeLabels                  map[string]string `json:"node_labels"`
		NodeTaints                  []poolTaint       `json:"node_taints"`
	}
	if !decodeStrict(body, &req) {
		apiError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	des := 1
	if req.DesiredNodes != nil {
		des = *req.DesiredNodes
	}
	min, max := des, des
	if req.MinNodes != nil {
		min = *req.MinNodes
	}
	if req.MaxNodes != nil {
		max = *req.MaxNodes
	}
	var d []map[string]any
	d = append(d, validName(req.Name)...)
	if req.DiskGB < 1 || req.DiskGB > 2048 {
		d = append(d, detail("disk_gb", "invalid", "disk_gb must be between 1 and 2048"))
	}
	d = append(d, validateSize(req.Vcpus, req.MemoryGB)...)
	d = append(d, validateBounds(min, des, max)...)
	d = append(d, validateBudget(req.Vcpus, req.MemoryGB, req.MinVcpus, req.MaxVcpus, req.MinMemoryGB, req.MaxMemoryGB)...)
	d = append(d, validateKnobs(req.DrainTimeoutSeconds, req.ScaleDownDelaySeconds, req.ScaleDownUtilizationPercent)...)
	for k := range req.NodeLabels {
		if k == "" {
			d = append(d, detail("node_labels", "invalid", "label key must not be empty"))
		} else if hasReserved(k) {
			d = append(d, detail("node_labels", "reserved", "label key "+k+" uses a reserved prefix"))
		}
	}
	seen := map[string]bool{}
	for _, t := range req.NodeTaints {
		switch {
		case t.Key == "":
			d = append(d, detail("node_taints", "invalid", "taint key must not be empty"))
		case hasReserved(t.Key):
			d = append(d, detail("node_taints", "reserved", "taint key "+t.Key+" uses a reserved prefix"))
		case t.Effect != "NoSchedule" && t.Effect != "PreferNoSchedule" && t.Effect != "NoExecute":
			d = append(d, detail("node_taints", "invalid", "taint effect must be NoSchedule, PreferNoSchedule or NoExecute"))
		case seen[t.Key]:
			d = append(d, detail("node_taints", "duplicate", "duplicate taint key "+t.Key))
		}
		seen[t.Key] = true
	}
	if d != nil {
		validationError(w, d)
		return
	}
	blocks := false
	for _, t := range req.NodeTaints {
		blocks = blocks || blocksScheduling(t)
	}
	if blocks {
		ok := false
		for _, o := range ps.sorted(cid) {
			free := !o.deleting
			for _, t := range o.taints {
				free = free && !blocksScheduling(t)
			}
			ok = ok || free
		}
		if !ok {
			apiError(w, http.StatusBadRequest, "no_schedulable_pool", "at least one other pool must remain schedulable")
			return
		}
	}
	if ps.nameTaken(cid, req.Name, "") {
		apiError(w, http.StatusConflict, "conflict", "Node pool already exists")
		return
	}
	ps.next++
	p := &fakePool{
		id: uuid.NewString(), clusterID: cid, name: req.Name,
		vcpus: req.Vcpus, memoryGB: req.MemoryGB, diskGB: req.DiskGB,
		minNodes: min, maxNodes: max, desired: des,
		minVCPUs: req.MinVcpus, maxVCPUs: req.MaxVcpus, minMemGB: req.MinMemoryGB, maxMemGB: req.MaxMemoryGB,
		labels: req.NodeLabels, taints: req.NodeTaints,
		drain: req.DrainTimeoutSeconds, scaleDelay: req.ScaleDownDelaySeconds, scaleUtil: req.ScaleDownUtilizationPercent,
		seq: ps.next,
	}
	ps.pools[p.id] = p
	if ps.knobs.nonJSONCreate {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
		return
	}
	// Always "provisioning", current 0 on the POST, independent of the GET sequence.
	writeJSON(w, http.StatusAccepted, ps.poolJSON(p, "provisioning", 0))
}

// nameTaken: a pool in "deleting" still holds its name until a GET sweeps it.
func (ps *poolStore) nameTaken(cid, name, exceptID string) bool {
	for _, o := range ps.sorted(cid) {
		if o.name == name && o.id != exceptID {
			return true
		}
	}
	return false
}

// read is the status/current_nodes one GET reports, advancing the fake's state:
// the sequence walk, first-ready materialisation and an in-flight scaling walk.
func (ps *poolStore) read(p *fakePool) (string, int) {
	base := "running"
	if seq, ok := lookup(ps.knobs.statuses, p.id); ok && len(seq) > 0 {
		i := p.reads
		if i > len(seq)-1 {
			i = len(seq) - 1
		}
		base = seq[i]
	}
	p.reads++
	if base == "running" && !p.everRunning {
		p.everRunning = true
		p.current = p.desired
		p.mutations++
		if p.current > 0 {
			p.firstReady = true
		}
	}
	if !p.everRunning {
		return base, 0
	}
	if p.scaling && base == "running" {
		p.scaleReads++
		switch {
		case p.scaleReads <= ps.knobs.lagGets:
		case p.scaleReads <= ps.knobs.lagGets+ps.knobs.lagScaling:
			if p.scalingUp {
				return "scaling", p.current
			}
			return "scaling-down", p.current
		default:
			p.scaling = false
			p.current = p.desired
			p.firstReady = p.firstReady || p.current > 0
			p.mutations++
		}
	}
	return base, p.current
}

func (ps *poolStore) get(w http.ResponseWriter, p *fakePool) {
	if p.deleting {
		if f, ok := lookup(ps.knobs.getFailAfterD, p.id); ok {
			apiError(w, f.code, codeFor(f.code), f.message)
			return
		}
		if p.deleteGetsLeft <= 0 {
			delete(ps.pools, p.id)
			apiError(w, http.StatusNotFound, "not_found", "Resource not found")
			return
		}
		p.deleteGetsLeft--
		writeJSON(w, http.StatusOK, ps.poolJSON(p, "deleting", p.current))
		return
	}
	st, cur := ps.read(p)
	writeJSON(w, http.StatusOK, ps.poolJSON(p, st, cur))
}

func (ps *poolStore) put(w http.ResponseWriter, p *fakePool, body []byte) {
	var req struct {
		Name                        *string `json:"name"`
		Vcpus                       *int    `json:"vcpus"`
		MemoryGB                    *int    `json:"memory_gb"`
		DiskGB                      *int    `json:"disk_gb"`
		MinNodes                    *int    `json:"min_nodes"`
		MaxNodes                    *int    `json:"max_nodes"`
		DesiredNodes                *int    `json:"desired_nodes"`
		MinVcpus                    *int    `json:"min_vcpus"`
		MaxVcpus                    *int    `json:"max_vcpus"`
		MinMemoryGB                 *int    `json:"min_memory_gb"`
		MaxMemoryGB                 *int    `json:"max_memory_gb"`
		DrainTimeoutSeconds         *int    `json:"drain_timeout_seconds"`
		ScaleDownDelaySeconds       *int    `json:"scale_down_delay_seconds"`
		ScaleDownUtilizationPercent *int    `json:"scale_down_utilization_percent"`
	}
	if !decodeStrict(body, &req) {
		apiError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	or := func(v *int, def int) int {
		if v != nil {
			return *v
		}
		return def
	}
	// Step 1: what the request itself says.
	var d []map[string]any
	if req.Name != nil {
		d = append(d, validName(*req.Name)...)
	}
	for f, v := range map[string]*int{"min_nodes": req.MinNodes, "desired_nodes": req.DesiredNodes, "max_nodes": req.MaxNodes} {
		if v != nil && *v < 0 {
			d = append(d, detail(f, "invalid", f+" must be >= 0"))
		}
	}
	d = append(d, validateKnobs(req.DrainTimeoutSeconds, req.ScaleDownDelaySeconds, req.ScaleDownUtilizationPercent)...)
	// Step 2: the request merged over the STORED row (min/max from stored, NOT
	// from the request's desired - the API's known mismatch with the SQL below).
	vcpus, mem := or(req.Vcpus, p.vcpus), or(req.MemoryGB, p.memoryGB)
	d = append(d, validateBounds(or(req.MinNodes, p.minNodes), or(req.DesiredNodes, p.desired), or(req.MaxNodes, p.maxNodes))...)
	d = append(d, validateSize(vcpus, mem)...)
	minV, maxV := or(req.MinVcpus, p.minVCPUs), or(req.MaxVcpus, p.maxVCPUs)
	minM, maxM := or(req.MinMemoryGB, p.minMemGB), or(req.MaxMemoryGB, p.maxMemGB)
	d = append(d, validateBudget(vcpus, mem, minV, maxV, minM, maxM)...)
	if d != nil {
		validationError(w, d)
		return
	}
	if req.Name != nil && ps.nameTaken(p.clusterID, *req.Name, p.id) {
		apiError(w, http.StatusConflict, "conflict", "Node pool already exists")
		return
	}
	if req.DiskGB != nil && (*req.DiskGB < 1 || *req.DiskGB > 2048) {
		// never validated by the API on update; the DB CHECK fails as a 500
		apiError(w, http.StatusInternalServerError, "internal_error", "Failed to update node pool")
		return
	}
	// The write: min/max fall back to the request's desired_nodes (COALESCE($min,$desired,min)).
	newMin := or(req.MinNodes, or(req.DesiredNodes, p.minNodes))
	newMax := or(req.MaxNodes, or(req.DesiredNodes, p.maxNodes))
	if req.Name != nil {
		p.name = *req.Name
	}
	p.vcpus, p.memoryGB = vcpus, mem
	p.diskGB = or(req.DiskGB, p.diskGB)
	p.minNodes, p.maxNodes, p.desired = newMin, newMax, or(req.DesiredNodes, p.desired)
	p.minVCPUs, p.maxVCPUs, p.minMemGB, p.maxMemGB = minV, maxV, minM, maxM
	// null == omitted == keep: these can never be cleared.
	if req.DrainTimeoutSeconds != nil {
		p.drain = req.DrainTimeoutSeconds
	}
	if req.ScaleDownDelaySeconds != nil {
		p.scaleDelay = req.ScaleDownDelaySeconds
	}
	if req.ScaleDownUtilizationPercent != nil {
		p.scaleUtil = req.ScaleDownUtilizationPercent
	}
	p.mutations++
	// The reconciler, later: a fixed pool whose ready count differs from desired
	// walks scaling/scaling-down; an autoscaling pool never does. status is NOT
	// touched here.
	switch {
	case p.autoscaling() || p.deleting || !p.everRunning:
		p.scaling = false
	case p.desired != p.current:
		p.scaling, p.scaleReads, p.scalingUp = true, 0, p.desired > p.current
	default:
		p.scaling = false
	}
	st, cur := ps.peek(p)
	writeJSON(w, http.StatusOK, ps.poolJSON(p, st, cur))
}
