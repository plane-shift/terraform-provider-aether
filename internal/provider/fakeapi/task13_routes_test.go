package fakeapi

// Fake-level tests for the Task 13 routes: /regions, /k8s-versions,
// …/kubeconfig, …/loadbalancers, …/volumes, …/nodepools/{id}/machines, plus
// the cluster-list paging fix (clusters.go previously served every cluster
// unconditionally with no cursor/limit support at all).

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// callNoAuth is call() without the Authorization header, for proving a route
// is genuinely public (contract §1/§2 - security: []).
func callNoAuth(t *testing.T, s *Server, method, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, s.URL()+"/api/v1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// ---------------------------------------------------------------- /regions

// Fails if /regions demands a bearer token, returns anything but exactly one
// eu-fi region, or ever sets available=false (this fake models a single
// eu-fi-serving instance).
func TestRegionsIsPublicAndReturnsExactlyOneAvailableRegion(t *testing.T) {
	s := newClusterFake(t)
	resp := callNoAuth(t, s, http.MethodGet, "/regions")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("no-auth GET /regions = %d %s, want 200 (public route)", resp.StatusCode, b)
	}
	m := decode(t, b)
	items := m["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want exactly one region", items)
	}
	r := items[0].(map[string]any)
	if r["id"] != "eu-fi" || r["name"] != "Europe (Finland)" || r["location"] != "Helsinki" || r["available"] != true {
		t.Fatalf("region = %v", r)
	}
}

// ---------------------------------------------------------------- /k8s-versions

// Fails if /k8s-versions demands a bearer token, uses the wrong envelope key
// ("versions" instead of "items"), puts "Latest" anywhere but the first item,
// or reorders away from newest-first.
func TestK8sVersionsIsPublicNewestFirstWithLatestOnFirstItemOnly(t *testing.T) {
	s := newClusterFake(t)
	resp := callNoAuth(t, s, http.MethodGet, "/k8s-versions")
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("no-auth GET /k8s-versions = %d %s, want 200 (public route)", resp.StatusCode, b)
	}
	m := decode(t, b)
	if _, has := m["versions"]; has {
		t.Fatalf("body has a \"versions\" key, the real key is \"items\": %s", b)
	}
	items := m["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %v, want the 3-item default table", items)
	}
	first := items[0].(map[string]any)
	if first["version"] != "1.37" || first["label"] != "Latest" || first["patch"] != "1.37.2" {
		t.Fatalf("first item = %v", first)
	}
	for i, it := range items[1:] {
		m := it.(map[string]any)
		if m["label"] != "Stable" {
			t.Fatalf("item %d label = %v, want Stable (only item 0 may be Latest)", i+1, m["label"])
		}
	}
}

// Fails if SetK8sVersions() with zero arguments does not produce exactly
// 200 {"items":[]} - the fake's knob for "the platform's version window is
// unavailable right now" (contract §2: NEVER a 503).
func TestK8sVersionsSetToEmptyAnswers200WithEmptyItemsNever503(t *testing.T) {
	s := newClusterFake(t)
	s.SetK8sVersions()
	resp, b := call(t, s, http.MethodGet, "/k8s-versions", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (never 503) for an unavailable window", resp.StatusCode)
	}
	m := decode(t, b)
	items, ok := m["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("items = %s, want an explicit empty list", b)
	}
}

// Fails if a custom SetK8sVersions list is not served verbatim in the given
// order (the route must never re-sort).
func TestK8sVersionsCustomListIsServedInGivenOrder(t *testing.T) {
	s := newClusterFake(t)
	s.SetK8sVersions(
		K8sVersion{Version: "1.38", Label: "Latest", Patch: "1.38.1"},
		K8sVersion{Version: "1.37", Label: "Stable", Patch: "1.37.9"},
	)
	_, b := call(t, s, http.MethodGet, "/k8s-versions", "")
	items := decode(t, b)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["patch"] != "1.38.1" || first["label"] != "Latest" {
		t.Fatalf("first = %v", first)
	}
	if second["patch"] != "1.37.9" || second["label"] != "Stable" {
		t.Fatalf("second = %v", second)
	}
}

// ---------------------------------------------------------------- kubeconfig

func kubeconfigPath(cid string) string { return clustersPath + "/" + cid + "/kubeconfig" }

// Fails if the 200 body carries any JSON envelope/key at all (it must be raw
// YAML bytes), if either header is missing or wrong, or if a running cluster
// answers anything but 200.
func TestKubeconfigRunningClusterServesRawYAMLWithTheTwoHeaders(t *testing.T) {
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	getStatus(t, s, cid) // first GET of the cluster -> "running" (no sequence set)
	s.SetKubeconfig(cid, "apiVersion: v1\nkind: Config\n")
	resp, b := call(t, s, http.MethodGet, kubeconfigPath(cid), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d %s, want 200", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/yaml" {
		t.Fatalf("Content-Type = %q, want application/yaml", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="kubeconfig.yaml"` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if string(b) != "apiVersion: v1\nkind: Config\n" {
		t.Fatalf("body = %q, want the seeded YAML verbatim with no JSON wrapping", b)
	}
	// A JSON-wrapped body (the plan's rejected {"kubeconfig": "..."} shape)
	// would start with '{'; the real wire format never does.
	if strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
		t.Fatalf("body looks JSON-wrapped: %q, want raw YAML with no envelope", b)
	}
}

// Fails if an unseeded cluster's kubeconfig is empty, or if two different
// clusters serve the same bytes (defaultKubeconfigYAML must be
// per-cluster-deterministic, not a shared constant).
func TestKubeconfigUnseededClusterServesADeterministicPerClusterDefault(t *testing.T) {
	s := newClusterFake(t)
	a := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	b := create(t, s, `{"name":"b","region":"r","k8s_version":"1.37"}`)["id"].(string)
	getStatus(t, s, a)
	getStatus(t, s, b)
	_, ya := call(t, s, http.MethodGet, kubeconfigPath(a), "")
	_, yb := call(t, s, http.MethodGet, kubeconfigPath(b), "")
	if len(ya) == 0 || len(yb) == 0 {
		t.Fatal("unseeded kubeconfig body is empty")
	}
	if string(ya) == string(yb) {
		t.Fatalf("two different clusters served identical kubeconfig bytes: %q", ya)
	}
}

// Fails if any non-running status (including "upgrading"/"degraded", not
// just "provisioning") answers anything but 409 not_ready with the exact
// message shape, or if the message omits the actual status.
func TestKubeconfigNonRunningStatusIs409ForEveryStatus(t *testing.T) {
	for _, status := range []string{"provisioning", "upgrading", "degraded", "deleting", "failed"} {
		t.Run(status, func(t *testing.T) {
			s := newClusterFake(t)
			cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
			s.StatusSequence("*", status)
			resp, b := call(t, s, http.MethodGet, kubeconfigPath(cid), "")
			if resp.StatusCode != 409 {
				t.Fatalf("status %s: kubeconfig GET = %d %s, want 409", status, resp.StatusCode, b)
			}
			m := decode(t, b)["error"].(map[string]any)
			if m["code"] != "not_ready" {
				t.Fatalf("code = %v, want not_ready", m["code"])
			}
			want := "Cluster is not yet ready. Current status: " + status
			if m["message"] != want {
				t.Fatalf("message = %q, want %q", m["message"], want)
			}
		})
	}
}

// Fails if reading the kubeconfig advances the cluster's own StatusSequence
// walk (it is a different route; a GET of it must not consume a step meant
// for GET …/clusters/{id}).
func TestKubeconfigReadDoesNotAdvanceTheClusterStatusSequence(t *testing.T) {
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	s.StatusSequence("*", "provisioning", "running")
	for i := 0; i < 5; i++ {
		call(t, s, http.MethodGet, kubeconfigPath(cid), "") // always 409 (still "provisioning", never advances)
	}
	if got := getStatus(t, s, cid); got != "provisioning" {
		t.Fatalf("cluster status after 5 kubeconfig reads = %s, want provisioning (unconsumed)", got)
	}
}

// Fails if an unknown cluster id (or a cluster in a different org) answers
// anything but 404.
func TestKubeconfigUnknownClusterIs404(t *testing.T) {
	s := newClusterFake(t)
	resp, _ := call(t, s, http.MethodGet, clustersPath+"/"+notARealUUID+"/kubeconfig", "")
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

const notARealUUID = "99999999-9999-4999-8999-999999999999"

// ---------------------------------------------------------------- loadbalancers

func loadbalancersPath(cid string) string { return clustersPath + "/" + cid + "/loadbalancers" }

// Fails if the fake ever reproduces the stale OA/CG shape
// (service_name/namespace/external_ip/dns_name/ports) instead of the real
// one, if service_ref/allocated_at are present as null instead of omitted, or
// if the order is not allocated_at NULLS LAST then ip.
func TestLoadBalancersRealShapeAndOrdering(t *testing.T) {
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	uid := "svc-uid-123"
	later := "2026-09-20T00:00:00Z"
	earlier := "2026-09-10T00:00:00Z"
	// Seeded deliberately out of the real order to prove the route re-sorts.
	s.SeedLoadBalancers(cid,
		LoadBalancer{IP: "10.0.0.2", Purpose: "ccm_lb", ServiceRef: &uid, AllocatedAt: &later},
		LoadBalancer{IP: "10.0.0.9", Purpose: "cp_lb"}, // never allocated -> sorts LAST
		LoadBalancer{IP: "10.0.0.1", Purpose: "cp_lb", AllocatedAt: &earlier},
	)
	resp, b := call(t, s, http.MethodGet, loadbalancersPath(cid), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d %s", resp.StatusCode, b)
	}
	items := decode(t, b)["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %v", items)
	}
	first, second, third := items[0].(map[string]any), items[1].(map[string]any), items[2].(map[string]any)
	if first["ip"] != "10.0.0.1" || second["ip"] != "10.0.0.2" || third["ip"] != "10.0.0.9" {
		t.Fatalf("order = %v, want earliest-allocated first and never-allocated last", items)
	}
	for _, key := range []string{"service_name", "namespace", "external_ip", "dns_name", "ports"} {
		if _, has := first[key]; has {
			t.Fatalf("item carries stale OA/CG key %q: %v", key, first)
		}
	}
	if _, has := third["service_ref"]; has {
		t.Fatalf("cp_lb item carries service_ref, want it entirely absent: %v", third)
	}
	if _, has := third["allocated_at"]; has {
		t.Fatalf("never-allocated item carries allocated_at, want it entirely absent: %v", third)
	}
	if second["service_ref"] != uid {
		t.Fatalf("service_ref = %v, want %q", second["service_ref"], uid)
	}
}

// Fails if an unseeded cluster's loadbalancers is anything but an empty list,
// or if an unknown cluster answers anything but 404.
func TestLoadBalancersUnseededIsEmptyAndUnknownClusterIs404(t *testing.T) {
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	_, b := call(t, s, http.MethodGet, loadbalancersPath(cid), "")
	items, ok := decode(t, b)["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("items = %s, want an empty list", b)
	}
	resp, _ := call(t, s, http.MethodGet, clustersPath+"/"+notARealUUID+"/loadbalancers", "")
	if resp.StatusCode != 404 {
		t.Fatalf("unknown cluster = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------- volumes

func volumesPath(cid string) string { return clustersPath + "/" + cid + "/volumes" }

// Fails if the response is a bare list instead of the object envelope, if
// any of the five top-level keys is missing, or if a seeded volume's 11
// fields don't round-trip.
func TestVolumesIsAnObjectWithAllFieldsAndTheTwoOrgWideFigures(t *testing.T) {
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	s.SeedVolumes(cid, 1000, 400,
		Volume{ID: "vol-1", Name: "data", Type: "block", StorageClass: "aether-block",
			ProvisionedBytes: 500, UsedBytes: 200, UsedPercent: 40.0, BilledOn: "provisioned",
			State: "Bound", Watchers: 1, CreatedAt: "2026-09-01T00:00:00Z"},
	)
	resp, b := call(t, s, http.MethodGet, volumesPath(cid), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d %s", resp.StatusCode, b)
	}
	m := decode(t, b)
	if _, isList := m["items"]; isList {
		t.Fatalf("body carries an \"items\" key - volumes is an OBJECT, not a list envelope: %s", b)
	}
	for _, key := range []string{"volumes", "org_shared_quota_bytes", "org_shared_used_bytes", "inventory_available", "last_synced_at"} {
		if _, has := m[key]; !has {
			t.Fatalf("body missing key %q: %s", key, b)
		}
	}
	if m["inventory_available"] != true {
		t.Fatalf("inventory_available = %v, want true", m["inventory_available"])
	}
	if m["org_shared_quota_bytes"].(float64) != 1000 || m["org_shared_used_bytes"].(float64) != 400 {
		t.Fatalf("org-wide figures = %v/%v", m["org_shared_quota_bytes"], m["org_shared_used_bytes"])
	}
	vols := m["volumes"].([]any)
	if len(vols) != 1 {
		t.Fatalf("volumes = %v", vols)
	}
	v := vols[0].(map[string]any)
	want := map[string]any{
		"id": "vol-1", "name": "data", "type": "block", "storage_class": "aether-block",
		"provisioned_bytes": float64(500), "used_bytes": float64(200), "used_percent": 40.0,
		"billed_on": "provisioned", "state": "Bound", "watchers": float64(1), "created_at": "2026-09-01T00:00:00Z",
	}
	for k, wantV := range want {
		if v[k] != wantV {
			t.Fatalf("volume[%s] = %v, want %v (full volume: %v)", k, v[k], wantV, v)
		}
	}
}

// Fails if SetVolumesUnavailable does not force EXACTLY the "could not look"
// shape (empty volumes, both figures zeroed, inventory_available false, the
// Go zero time) regardless of what SeedVolumes holds, or if it answers
// anything but 200.
func TestVolumesUnavailableForcesTheCouldNotLookShapeEvenWithSeededVolumes(t *testing.T) {
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	s.SeedVolumes(cid, 1000, 400, Volume{ID: "vol-1", Name: "data"})
	s.SetVolumesUnavailable(cid, true)
	resp, b := call(t, s, http.MethodGet, volumesPath(cid), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d %s, want 200 (never an error)", resp.StatusCode, b)
	}
	m := decode(t, b)
	vols, ok := m["volumes"].([]any)
	if !ok || len(vols) != 0 {
		t.Fatalf("volumes = %v, want an empty list despite the seeded volume", m["volumes"])
	}
	if m["inventory_available"] != false {
		t.Fatalf("inventory_available = %v, want false", m["inventory_available"])
	}
	if m["last_synced_at"] != "0001-01-01T00:00:00Z" {
		t.Fatalf("last_synced_at = %v, want the Go zero time", m["last_synced_at"])
	}
	if m["org_shared_quota_bytes"].(float64) != 0 || m["org_shared_used_bytes"].(float64) != 0 {
		t.Fatalf("org-wide figures = %v/%v, want both zeroed", m["org_shared_quota_bytes"], m["org_shared_used_bytes"])
	}
}

// Fails if the volumes route re-sorts by id itself - S2 (task-13 brief)
// depends on the FAKE preserving seed order, so the data source (not the
// fake) is what the test proves does the sorting.
func TestVolumesRouteDoesNotSortItPreservesSeedOrder(t *testing.T) {
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	s.SeedVolumes(cid, 0, 0,
		Volume{ID: "zzz"}, Volume{ID: "aaa"}, Volume{ID: "mmm"},
	)
	_, b := call(t, s, http.MethodGet, volumesPath(cid), "")
	vols := decode(t, b)["volumes"].([]any)
	if len(vols) != 3 {
		t.Fatalf("volumes = %v", vols)
	}
	ids := []string{vols[0].(map[string]any)["id"].(string), vols[1].(map[string]any)["id"].(string), vols[2].(map[string]any)["id"].(string)}
	if strings.Join(ids, ",") != "zzz,aaa,mmm" {
		t.Fatalf("ids = %v, want the seeded order unchanged (zzz,aaa,mmm) - the fake must not sort", ids)
	}
}

// ---------------------------------------------------------------- machines

func machinesPath(cid, pid string) string {
	return clustersPath + "/" + cid + "/nodepools/" + pid + "/machines"
}

// Fails if any of the six fields is missing/wrong, or if the result is not
// sorted by name regardless of seed order.
func TestMachinesAllFieldsSortedByName(t *testing.T) {
	s, cid := newPoolFake(t)
	pid := createPool(t, s, cid, fixedPool)["id"].(string)
	s.SeedMachines(pid,
		Machine{Name: "worker-2", Status: "Ready", K8sVersion: "v1.37.2", InternalIP: "10.64.1.2", AgeSeconds: 3600, CreatedAt: "2026-09-27T00:00:00Z"},
		Machine{Name: "worker-1", Status: "Provisioning", K8sVersion: "", InternalIP: "", AgeSeconds: 0, CreatedAt: "0001-01-01T00:00:00Z"},
	)
	resp, b := call(t, s, http.MethodGet, machinesPath(cid, pid), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d %s", resp.StatusCode, b)
	}
	items := decode(t, b)["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["name"] != "worker-1" || second["name"] != "worker-2" {
		t.Fatalf("order = %v, want sorted by name (worker-1, worker-2)", items)
	}
	if second["status"] != "Ready" || second["k8s_version"] != "v1.37.2" || second["internal_ip"] != "10.64.1.2" ||
		second["age_seconds"].(float64) != 3600 || second["created_at"] != "2026-09-27T00:00:00Z" {
		t.Fatalf("worker-2 = %v", second)
	}
}

// Fails if SetMachinesUnavailable does not force EXACTLY 200 {"items":[]}
// regardless of what SeedMachines holds.
func TestMachinesUnavailableForces200EmptyEvenWithSeededMachines(t *testing.T) {
	s, cid := newPoolFake(t)
	pid := createPool(t, s, cid, fixedPool)["id"].(string)
	s.SeedMachines(pid, Machine{Name: "worker-1", Status: "Ready"})
	s.SetMachinesUnavailable(pid, true)
	resp, b := call(t, s, http.MethodGet, machinesPath(cid, pid), "")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d %s, want 200", resp.StatusCode, b)
	}
	items, ok := decode(t, b)["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("items = %v, want an empty list despite the seeded machine", items)
	}
}

// Fails if SetMachinesListFails does not answer exactly 502 kube_list_failed.
func TestMachinesListFailsAnswers502KubeListFailed(t *testing.T) {
	s, cid := newPoolFake(t)
	pid := createPool(t, s, cid, fixedPool)["id"].(string)
	s.SetMachinesListFails(pid, true)
	resp, b := call(t, s, http.MethodGet, machinesPath(cid, pid), "")
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d %s, want 502", resp.StatusCode, b)
	}
	if errCode(decode(t, b)) != "kube_list_failed" {
		t.Fatalf("body = %s, want error.code = kube_list_failed", b)
	}
}

// Fails if an unknown pool (or one belonging to a different cluster) answers
// anything but 404.
func TestMachinesUnknownPoolIs404(t *testing.T) {
	s, cid := newPoolFake(t)
	resp, _ := call(t, s, http.MethodGet, machinesPath(cid, notARealUUID), "")
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------- cluster-list paging

// Fails if GET /clusters ignores limit/cursor entirely (the pre-fix
// behaviour: every cluster in one unpaginated response), if next_cursor
// appears on a non-full page, or if it is missing on a full page.
func TestClusterListPaginatesByLimitAndCursorWithNextCursorOnlyOnAFullPage(t *testing.T) {
	s := newClusterFake(t)
	var ids []string
	for _, n := range []string{"a", "b", "c"} {
		ids = append(ids, create(t, s, `{"name":"`+n+`","region":"r","k8s_version":"1.37"}`)["id"].(string))
	}
	_, b := call(t, s, http.MethodGet, clustersPath+"?limit=2", "")
	m := decode(t, b)
	items := m["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != ids[0] || items[1].(map[string]any)["id"] != ids[1] || m["next_cursor"] != ids[1] {
		t.Fatalf("page 1 = %s, want [a b] with next_cursor = b", b)
	}
	_, b = call(t, s, http.MethodGet, clustersPath+"?limit=2&cursor="+ids[1], "")
	m = decode(t, b)
	items = m["items"].([]any)
	if _, has := m["next_cursor"]; len(items) != 1 || items[0].(map[string]any)["id"] != ids[2] || has {
		t.Fatalf("page 2 = %s, want [c] and no next_cursor", b)
	}
}

// Fails if a total that is an EXACT MULTIPLE of limit does not cost the
// documented trailing empty page (contract §7): 4 clusters at limit=2 must
// yield [a b] (next_cursor=b), [c d] (next_cursor=d), [] (no next_cursor).
func TestClusterListExactMultipleOfLimitCostsATrailingEmptyPage(t *testing.T) {
	s := newClusterFake(t)
	var ids []string
	for _, n := range []string{"a", "b", "c", "d"} {
		ids = append(ids, create(t, s, `{"name":"`+n+`","region":"r","k8s_version":"1.37"}`)["id"].(string))
	}
	_, b := call(t, s, http.MethodGet, clustersPath+"?limit=2", "")
	m := decode(t, b)
	if m["next_cursor"] != ids[1] {
		t.Fatalf("page 1 next_cursor = %v, want %v", m["next_cursor"], ids[1])
	}
	_, b = call(t, s, http.MethodGet, clustersPath+"?limit=2&cursor="+ids[1], "")
	m = decode(t, b)
	items := m["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != ids[2] || items[1].(map[string]any)["id"] != ids[3] || m["next_cursor"] != ids[3] {
		t.Fatalf("page 2 = %s, want [c d] with next_cursor = d", b)
	}
	_, b = call(t, s, http.MethodGet, clustersPath+"?limit=2&cursor="+ids[3], "")
	m = decode(t, b)
	items = m["items"].([]any)
	if _, has := m["next_cursor"]; len(items) != 0 || has {
		t.Fatalf("trailing page = %s, want an empty items list with no next_cursor", b)
	}
}

// Fails if SeedManyClusters produces IDs in a different order than the list
// route actually serves them in (a paging test built on top of it would then
// assert against the wrong expected order).
func TestSeedManyClustersOrderMatchesListOrder(t *testing.T) {
	s := newClusterFake(t)
	ids := s.SeedManyClusters(testOrg, 5)
	if len(ids) != 5 {
		t.Fatalf("got %d ids, want 5", len(ids))
	}
	_, b := call(t, s, http.MethodGet, clustersPath, "")
	items := decode(t, b)["items"].([]any)
	if len(items) != 5 {
		t.Fatalf("items = %v", items)
	}
	for i, it := range items {
		if it.(map[string]any)["id"] != ids[i] {
			t.Fatalf("item %d id = %v, want %v (SeedManyClusters order must match list order)", i, it.(map[string]any)["id"], ids[i])
		}
	}
}

// Fails if SeedManyNodePools produces IDs in a different order than the list
// route actually serves them in.
func TestSeedManyNodePoolsOrderMatchesListOrder(t *testing.T) {
	s, cid := newPoolFake(t)
	ids := s.SeedManyNodePools(cid, 5)
	if len(ids) != 5 {
		t.Fatalf("got %d ids, want 5", len(ids))
	}
	_, b := call(t, s, http.MethodGet, poolsPath(cid), "")
	items := decode(t, b)["items"].([]any)
	if len(items) != 5 {
		t.Fatalf("items = %v", items)
	}
	for i, it := range items {
		if it.(map[string]any)["id"] != ids[i] {
			t.Fatalf("item %d id = %v, want %v (SeedManyNodePools order must match list order)", i, it.(map[string]any)["id"], ids[i])
		}
	}
}

// Fails if an out-of-range or unparseable limit is not silently replaced with
// the default 50 (contract §7 - never a 400).
func TestClusterListBadLimitFallsBackToDefault50(t *testing.T) {
	s := newClusterFake(t)
	for i := 0; i < 3; i++ {
		create(t, s, `{"name":"c`+string(rune('a'+i))+`","region":"r","k8s_version":"1.37"}`)
	}
	for _, limit := range []string{"0", "101", "-5", "not-a-number"} {
		resp, b := call(t, s, http.MethodGet, clustersPath+"?limit="+limit, "")
		if resp.StatusCode != 200 {
			t.Fatalf("limit=%s: status = %d, want 200 (never 400)", limit, resp.StatusCode)
		}
		items := decode(t, b)["items"].([]any)
		if len(items) != 3 {
			t.Fatalf("limit=%s: items = %v, want all 3 (limit fell back to 50)", limit, items)
		}
	}
}
