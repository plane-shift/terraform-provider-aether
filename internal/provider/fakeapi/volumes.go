package fakeapi

// Cluster-volumes route of the fake (measured at aether-api 8e9045a; SDD
// task-13-measured-contract.md §5):
//
//	GET /organizations/{org}/clusters/{id}/volumes   200 {volumes[],org_shared_quota_bytes,org_shared_used_bytes,inventory_available,last_synced_at}
//
// An OBJECT, not a list. Not paginated. "Could not look" is ALWAYS a 200:
// SetVolumesUnavailable forces
// {"volumes":[],"org_shared_quota_bytes":0,"org_shared_used_bytes":0,
// "inventory_available":false,"last_synced_at":"0001-01-01T00:00:00Z"}
// regardless of what SeedVolumes holds - the real API collapses several
// distinct "could not look" causes onto exactly this one reply shape (§5).
// The route does NOT sort volumes: SeedVolumes' own order is served
// unchanged, so a test can seed out of id order and use that to prove the
// DATA SOURCE (not the fake) is the one that sorts by id (contract U2, T13-13).

import (
	"net/http"
	"regexp"
	"sync"
)

var volumesPathRE = regexp.MustCompile(`^/organizations/([^/]+)/clusters/([^/]+)/volumes$`)

// Volume is one item SeedVolumes seeds.
type Volume struct {
	ID, Name, Type, StorageClass, BilledOn, State string
	ProvisionedBytes, UsedBytes                   int64
	UsedPercent                                   float64
	Watchers                                      int
	CreatedAt                                     string
}

type volumeRow struct {
	volumes                                 []Volume
	orgSharedQuotaBytes, orgSharedUsedBytes int64
	unavailable                             bool
}

// volumeStore is embedded by value in Server; its zero value is usable.
type volumeStore struct {
	mu   sync.Mutex
	rows map[string]*volumeRow // keyed by clusterID
}

func (vs *volumeStore) init() {
	if vs.rows == nil {
		vs.rows = map[string]*volumeRow{}
	}
}

func (vs *volumeStore) rowFor(cid string) *volumeRow {
	vs.init()
	row := vs.rows[cid]
	if row == nil {
		row = &volumeRow{}
		vs.rows[cid] = row
	}
	return row
}

// SeedVolumes replaces the volumes list of clusterID and its two org-wide
// byte figures. Unseeded, a cluster reads an empty list with both figures 0.
func (s *Server) SeedVolumes(clusterID string, orgSharedQuotaBytes, orgSharedUsedBytes int64, volumes ...Volume) {
	s.volumes.mu.Lock()
	defer s.volumes.mu.Unlock()
	row := s.volumes.rowFor(clusterID)
	row.volumes = append([]Volume(nil), volumes...)
	row.orgSharedQuotaBytes, row.orgSharedUsedBytes = orgSharedQuotaBytes, orgSharedUsedBytes
}

// SetVolumesUnavailable forces the "could not look" 200 for clusterID
// regardless of what SeedVolumes holds. on=false clears the knob.
func (s *Server) SetVolumesUnavailable(clusterID string, on bool) {
	s.volumes.mu.Lock()
	defer s.volumes.mu.Unlock()
	s.volumes.rowFor(clusterID).unavailable = on
}

const zeroVolumesSyncedAt = "0001-01-01T00:00:00Z"

// routeVolumes serves GET …/volumes; it reports false when path is not that
// route.
func (s *Server) routeVolumes(w http.ResponseWriter, r *http.Request, path string, _ []byte) bool {
	m := volumesPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	org, cid := m[1], m[2]
	if !s.orgKnown(org) || !s.clusterInOrg(org, cid) {
		apiError(w, http.StatusNotFound, "not_found", "cluster not found")
		return true
	}
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		return true
	}
	s.volumes.mu.Lock()
	row := s.volumes.rowFor(cid)
	unavailable := row.unavailable
	vols := append([]Volume(nil), row.volumes...)
	quota, used := row.orgSharedQuotaBytes, row.orgSharedUsedBytes
	s.volumes.mu.Unlock()
	if unavailable {
		writeJSON(w, http.StatusOK, map[string]any{
			"volumes": []map[string]any{}, "org_shared_quota_bytes": int64(0), "org_shared_used_bytes": int64(0),
			"inventory_available": false, "last_synced_at": zeroVolumesSyncedAt,
		})
		return true
	}
	items := make([]map[string]any, 0, len(vols))
	for _, v := range vols {
		items = append(items, map[string]any{
			"id": v.ID, "name": v.Name, "type": v.Type, "storage_class": v.StorageClass,
			"provisioned_bytes": v.ProvisionedBytes, "used_bytes": v.UsedBytes, "used_percent": v.UsedPercent,
			"billed_on": v.BilledOn, "state": v.State, "watchers": v.Watchers, "created_at": v.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volumes": items, "org_shared_quota_bytes": quota, "org_shared_used_bytes": used,
		"inventory_available": true, "last_synced_at": "2026-09-28T00:00:00Z",
	})
	return true
}
