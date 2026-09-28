package fakeapi

// Node-pool machines route of the fake (measured at aether-api 8e9045a; SDD
// task-13-measured-contract.md §6):
//
//	GET /organizations/{org}/clusters/{id}/nodepools/{poolID}/machines   200 {"items":[{name,status,k8s_version,internal_ip,age_seconds,created_at}]}
//
// The route segment is "nodepools", never "node-pools" (R:384). Not
// paginated; sorted by name (MH:121 - the route re-sorts on every read, so a
// test can seed out of name order). "No CAPI name" / "no kube client" is
// ALWAYS a 200 [] (SetMachinesUnavailable), indistinguishable on the wire
// from "genuinely no machines"; a kube list error is a 502 kube_list_failed
// (SetMachinesListFails) - both knobs keyed on poolID (contract §6).

import (
	"net/http"
	"regexp"
	"sort"
	"sync"
)

var machinesPathRE = regexp.MustCompile(`^/organizations/([^/]+)/clusters/([^/]+)/nodepools/([^/]+)/machines$`)

// zeroMachineCreatedAt is what an unseeded/zero-value Machine.CreatedAt
// renders as (review m6, the same fallback as volumes.go's D2 fix).
const zeroMachineCreatedAt = "0001-01-01T00:00:00Z"

// Machine is one item SeedMachines seeds.
type Machine struct {
	Name, Status, K8sVersion, InternalIP, CreatedAt string
	AgeSeconds                                      int64
}

type machineRow struct {
	machines    []Machine
	unavailable bool // "no CAPI name" / "no kube client": always 200 []
	listFails   bool // kube list error: 502 kube_list_failed
}

// machineStore is embedded by value in Server; its zero value is usable.
type machineStore struct {
	mu   sync.Mutex
	rows map[string]*machineRow // keyed by poolID
}

func (ms *machineStore) init() {
	if ms.rows == nil {
		ms.rows = map[string]*machineRow{}
	}
}

func (ms *machineStore) rowFor(poolID string) *machineRow {
	ms.init()
	row := ms.rows[poolID]
	if row == nil {
		row = &machineRow{}
		ms.rows[poolID] = row
	}
	return row
}

// SeedMachines replaces the machines list GET …/machines serves for poolID.
// Unseeded, a pool serves an empty list.
func (s *Server) SeedMachines(poolID string, machines ...Machine) {
	s.machines.mu.Lock()
	defer s.machines.mu.Unlock()
	s.machines.rowFor(poolID).machines = append([]Machine(nil), machines...)
}

// SetMachinesUnavailable forces the "no CAPI name / no kube client" 200 []
// for poolID regardless of what SeedMachines holds. on=false clears it.
func (s *Server) SetMachinesUnavailable(poolID string, on bool) {
	s.machines.mu.Lock()
	defer s.machines.mu.Unlock()
	s.machines.rowFor(poolID).unavailable = on
}

// SetMachinesListFails forces a 502 kube_list_failed for poolID. on=false
// clears it.
func (s *Server) SetMachinesListFails(poolID string, on bool) {
	s.machines.mu.Lock()
	defer s.machines.mu.Unlock()
	s.machines.rowFor(poolID).listFails = on
}

// routeMachines serves GET …/nodepools/{poolID}/machines; it reports false
// when path is not that route.
func (s *Server) routeMachines(w http.ResponseWriter, r *http.Request, path string, _ []byte) bool {
	m := machinesPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	org, cid, pid := m[1], m[2], m[3]
	if !s.orgKnown(org) || !s.clusterInOrg(org, cid) {
		apiError(w, http.StatusNotFound, "not_found", "cluster not found")
		return true
	}
	s.pools.mu.Lock()
	p, ok := s.pools.pools[pid]
	poolOK := ok && p.clusterID == cid
	s.pools.mu.Unlock()
	if !poolOK {
		apiError(w, http.StatusNotFound, "not_found", "node pool not found")
		return true
	}
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		return true
	}
	s.machines.mu.Lock()
	row := s.machines.rowFor(pid)
	unavailable, listFails := row.unavailable, row.listFails
	all := append([]Machine(nil), row.machines...)
	s.machines.mu.Unlock()
	if listFails {
		apiError(w, http.StatusBadGateway, "kube_list_failed", "failed to list machines")
		return true
	}
	if unavailable {
		writeJSON(w, http.StatusOK, map[string]any{"items": []map[string]any{}})
		return true
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	items := make([]map[string]any, 0, len(all))
	for _, mc := range all {
		createdAt := mc.CreatedAt
		if createdAt == "" {
			// Same latent defect as D2 in the volumes fake (review m6): the
			// real API always emits an RFC3339 time, and an unseeded/zero-value
			// CreatedAt must still round-trip through the generated client's
			// time.Time field instead of failing to parse the empty string.
			createdAt = zeroMachineCreatedAt
		}
		items = append(items, map[string]any{
			"name": mc.Name, "status": mc.Status, "k8s_version": mc.K8sVersion,
			"internal_ip": mc.InternalIP, "age_seconds": mc.AgeSeconds, "created_at": createdAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return true
}
