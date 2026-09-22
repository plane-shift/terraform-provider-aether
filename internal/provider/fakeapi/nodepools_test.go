package fakeapi

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// newPoolFake returns a fake with one org and one cluster (still "provisioning"
// as its POST reported it: the API has no cluster-status precondition for pools).
func newPoolFake(t *testing.T) (*Server, string) {
	t.Helper()
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	return s, cid
}

func poolsPath(cid string) string { return clustersPath + "/" + cid + "/nodepools" }

const fixedPool = `{"name":"workers","vcpus":2,"memory_gb":4,"disk_gb":20}`

func createPool(t *testing.T, s *Server, cid, body string) map[string]any {
	t.Helper()
	resp, b := call(t, s, http.MethodPost, poolsPath(cid), body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST nodepools = %d %s, want 202", resp.StatusCode, b)
	}
	return decode(t, b)
}

func getPoolResp(t *testing.T, s *Server, cid, id string) (*http.Response, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodGet, poolsPath(cid)+"/"+id, "")
	return resp, decode(t, b)
}

func getPool(t *testing.T, s *Server, cid, id string) map[string]any {
	t.Helper()
	resp, m := getPoolResp(t, s, cid, id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET pool = %d %v, want 200", resp.StatusCode, m)
	}
	return m
}

func putPool(t *testing.T, s *Server, cid, id, body string) (int, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodPut, poolsPath(cid)+"/"+id, body)
	return resp.StatusCode, decode(t, b)
}

func num(m map[string]any, k string) int { return int(m[k].(float64)) }

func errCode(m map[string]any) string { return m["error"].(map[string]any)["code"].(string) }

func TestPoolCreateAnswers202WithProvisioningAndTheAPIsDefaults(t *testing.T) {
	s, cid := newPoolFake(t)
	p := createPool(t, s, cid, fixedPool)
	if _, err := uuid.Parse(p["id"].(string)); err != nil {
		t.Fatalf("id %v is not a UUID", p["id"])
	}
	if p["cluster_id"] != cid || p["name"] != "workers" || p["status"] != "provisioning" || num(p, "current_nodes") != 0 {
		t.Fatalf("body = %v", p)
	}
	if num(p, "desired_nodes") != 1 || num(p, "min_nodes") != 1 || num(p, "max_nodes") != 1 {
		t.Fatalf("counts = %v, want 1/1/1 defaults", p)
	}
	for _, k := range []string{"min_vcpus", "max_vcpus", "min_memory_gb", "max_memory_gb"} {
		if v, ok := p[k]; !ok || v.(float64) != 0 {
			t.Fatalf("%s = %v (present=%v), want a present 0", k, v, ok)
		}
	}
	if l, ok := p["node_labels"].(map[string]any); !ok || len(l) != 0 {
		t.Fatalf("node_labels = %#v, want {}", p["node_labels"])
	}
	if l, ok := p["node_taints"].([]any); !ok || len(l) != 0 {
		t.Fatalf("node_taints = %#v, want []", p["node_taints"])
	}
	for _, k := range []string{"drain_timeout_seconds", "scale_down_delay_seconds", "scale_down_utilization_percent", "first_ready_at", "status_message"} {
		if _, has := p[k]; has {
			t.Fatalf("%s = %v present, want the key absent when unset", k, p[k])
		}
	}
}

func TestPoolCreateEchoesCountsBudgetKnobsLabelsAndTaintsWithEmptyValue(t *testing.T) {
	s, cid := newPoolFake(t)
	p := createPool(t, s, cid, `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"desired_nodes":3,"drain_timeout_seconds":120,
		"scale_down_delay_seconds":300,"scale_down_utilization_percent":40,"node_labels":{"k":"v"},
		"node_taints":[{"key":"t","effect":"PreferNoSchedule"}]}`)
	if num(p, "desired_nodes") != 3 || num(p, "min_nodes") != 3 || num(p, "max_nodes") != 3 {
		t.Fatalf("min/max must default to desired: %v", p)
	}
	if num(p, "drain_timeout_seconds") != 120 || num(p, "scale_down_delay_seconds") != 300 || num(p, "scale_down_utilization_percent") != 40 {
		t.Fatalf("knobs = %v", p)
	}
	if p["node_labels"].(map[string]any)["k"] != "v" {
		t.Fatalf("labels = %v", p["node_labels"])
	}
	tt := p["node_taints"].([]any)[0].(map[string]any)
	if v, ok := tt["value"]; !ok || v != "" || tt["key"] != "t" || tt["effect"] != "PreferNoSchedule" {
		t.Fatalf("taint = %v, want value \"\" echoed when omitted", tt)
	}
	z := createPool(t, s, cid, `{"name":"zero","vcpus":1,"memory_gb":1,"disk_gb":10,"desired_nodes":0}`)
	if num(z, "desired_nodes") != 0 || num(z, "min_nodes") != 0 || num(z, "max_nodes") != 0 {
		t.Fatalf("explicit desired 0 = %v, want a legal 0/0/0", z)
	}
}

func TestPoolCreateValidationIs422WithFieldDetails(t *testing.T) {
	cases := map[string]string{
		"name":                     `{"name":"Bad_Name","vcpus":2,"memory_gb":4,"disk_gb":20}`,
		"vcpus":                    `{"name":"a","vcpus":0,"memory_gb":4,"disk_gb":20}`,
		"memory_gb":                `{"name":"a","vcpus":2,"memory_gb":513,"disk_gb":20}`,
		"disk_gb":                  `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":2049}`,
		"desired_nodes":            `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"min_nodes":2,"desired_nodes":1,"max_nodes":3}`,
		"max_vcpus":                `{"name":"a","vcpus":4,"memory_gb":4,"disk_gb":20,"min_vcpus":4,"max_vcpus":2,"min_memory_gb":4,"max_memory_gb":8}`,
		"drain_timeout_seconds":    `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"drain_timeout_seconds":10}`,
		"node_labels":              `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"node_labels":{"aether.cloud/x":"y"}}`,
		"node_taints":              `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"node_taints":[{"key":"k","effect":"Sometimes"}]}`,
		"scale_down_delay_seconds": `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"scale_down_delay_seconds":59}`,
	}
	for field, body := range cases {
		s, cid := newPoolFake(t)
		resp, b := call(t, s, http.MethodPost, poolsPath(cid), body)
		m := decode(t, b)
		if resp.StatusCode != 422 || errCode(m) != "validation_error" || resp.Header.Get("X-Request-ID") == "" || bytes.Contains(b, []byte("request_id")) {
			t.Fatalf("%s: = %d %s (X-Request-ID %q), want 422 validation_error with the id only in the header", field, resp.StatusCode, b, resp.Header.Get("X-Request-ID"))
		}
		details := m["error"].(map[string]any)["details"].([]any)
		found := false
		for _, d := range details {
			if d.(map[string]any)["field"] == field {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: details %v do not name the field", field, details)
		}
	}
}

func TestPoolCreateAcceptsAMaxBudgetOfZeroAndUnknownJSONKeysAre400(t *testing.T) {
	s, cid := newPoolFake(t)
	createPool(t, s, cid, `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"min_vcpus":0,"max_vcpus":0,"min_memory_gb":0,"max_memory_gb":0}`)
	resp, b := call(t, s, http.MethodPost, poolsPath(cid), `{"name":"b","vcpus":2,"memory_gb":4,"disk_gb":20,"bogus":1}`)
	if resp.StatusCode != 400 || errCode(decode(t, b)) != "bad_request" {
		t.Fatalf("unknown key = %d %s, want 400 bad_request", resp.StatusCode, b)
	}
}

func TestPoolDuplicateNameIs409EvenWhileTheOldPoolIsDeleting(t *testing.T) {
	s, cid := newPoolFake(t)
	p := createPool(t, s, cid, fixedPool)
	resp, b := call(t, s, http.MethodPost, poolsPath(cid), fixedPool)
	if resp.StatusCode != 409 || errCode(decode(t, b)) != "conflict" {
		t.Fatalf("duplicate = %d %s, want 409 conflict", resp.StatusCode, b)
	}
	s.NodePoolDeleteBecomes404After("*", 1)
	call(t, s, http.MethodDelete, poolsPath(cid)+"/"+p["id"].(string), "")
	if resp, b := call(t, s, http.MethodPost, poolsPath(cid), fixedPool); resp.StatusCode != 409 {
		t.Fatalf("name reuse while the old pool is deleting = %d %s, want 409", resp.StatusCode, b)
	}
	getPool(t, s, cid, p["id"].(string))                                // deleting
	call(t, s, http.MethodGet, poolsPath(cid)+"/"+p["id"].(string), "") // 404, swept
	createPool(t, s, cid, fixedPool)
}

func TestPoolRoutes404ForUnknownOrgClusterAndPool(t *testing.T) {
	s, cid := newPoolFake(t)
	other := "22222222-2222-4222-8222-222222222222"
	for _, path := range []string{
		"/organizations/" + other + "/clusters/" + cid + "/nodepools",
		poolsPath(uuid.NewString()),
		poolsPath(cid) + "/" + uuid.NewString(),
	} {
		resp, b := call(t, s, http.MethodGet, path, "")
		if resp.StatusCode != 404 || errCode(decode(t, b)) != "not_found" {
			t.Fatalf("GET %s = %d %s, want 404 not_found", path, resp.StatusCode, b)
		}
	}
	if resp, _ := call(t, s, http.MethodPost, poolsPath(uuid.NewString()), fixedPool); resp.StatusCode != 404 {
		t.Fatalf("create on an unknown cluster = %d, want 404", resp.StatusCode)
	}
}

func TestPoolCreateOnANoScheduleTaintNeedsAnotherSchedulablePool(t *testing.T) {
	s, cid := newPoolFake(t)
	tainted := `{"name":"t","vcpus":2,"memory_gb":4,"disk_gb":20,"node_taints":[{"key":"k","effect":"NoSchedule"}]}`
	resp, b := call(t, s, http.MethodPost, poolsPath(cid), tainted)
	if resp.StatusCode != 400 || errCode(decode(t, b)) != "no_schedulable_pool" {
		t.Fatalf("first pool with a NoSchedule taint = %d %s, want 400 no_schedulable_pool", resp.StatusCode, b)
	}
	createPool(t, s, cid, fixedPool)
	createPool(t, s, cid, tainted)
}

func TestNodePoolStatusSequenceWalksMessageOnlyWhileDegradedAndCurrentFollowsRunning(t *testing.T) {
	s, cid := newPoolFake(t)
	s.NodePoolStatusSequence("*", "provisioning", "degraded", "running")
	s.SetNodePoolStatusMessage("*", "Available=False: nodes not ready")
	id := createPool(t, s, cid, `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"desired_nodes":2}`)["id"].(string)
	var got []string
	for i := 0; i < 4; i++ {
		p := getPool(t, s, cid, id)
		got = append(got, p["status"].(string))
		_, hasMsg := p["status_message"]
		_, hasReady := p["first_ready_at"]
		switch p["status"] {
		case "provisioning":
			if hasMsg || hasReady || num(p, "current_nodes") != 0 {
				t.Fatalf("provisioning read = %v", p)
			}
		case "degraded":
			if p["status_message"] != "Available=False: nodes not ready" || hasReady || num(p, "current_nodes") != 0 {
				t.Fatalf("degraded read = %v, want the message and no nodes yet", p)
			}
		case "running":
			if hasMsg || !hasReady || num(p, "current_nodes") != 2 {
				t.Fatalf("running read = %v, want no message, first_ready_at, current 2", p)
			}
		}
	}
	if strings.Join(got, ",") != "provisioning,degraded,running,running" {
		t.Fatalf("statuses = %v", got)
	}
}

func TestNodePoolStatusSequenceDefaultsToRunningAndASpecificIDBeatsWildcard(t *testing.T) {
	s, cid := newPoolFake(t)
	a := createPool(t, s, cid, fixedPool)["id"].(string)
	b := createPool(t, s, cid, `{"name":"b","vcpus":2,"memory_gb":4,"disk_gb":20}`)["id"].(string)
	if got := getPool(t, s, cid, a)["status"]; got != "running" {
		t.Fatalf("default = %v, want running", got)
	}
	s.NodePoolStatusSequence("*", "degraded")
	s.NodePoolStatusSequence(b, "provisioning")
	if ga, gb := getPool(t, s, cid, a)["status"], getPool(t, s, cid, b)["status"]; ga != "degraded" || gb != "provisioning" {
		t.Fatalf("a=%v b=%v, want degraded/provisioning", ga, gb)
	}
}

func TestPoolPutNeverChangesStatusAndScalingLagsByGetCount(t *testing.T) {
	s, cid := newPoolFake(t)
	id := createPool(t, s, cid, fixedPool)["id"].(string)
	getPool(t, s, cid, id) // running, current 1
	code, put := putPool(t, s, cid, id, `{"min_nodes":3,"desired_nodes":3,"max_nodes":3}`)
	if code != 200 || put["status"] != "running" || num(put, "current_nodes") != 1 || num(put, "desired_nodes") != 3 {
		t.Fatalf("PUT = %d %v, want 200, still running, current 1, desired 3", code, put)
	}
	// default lag: 1 read still running/old, 1 read scaling, then converged
	want := []struct {
		status string
		cur    int
	}{{"running", 1}, {"scaling", 1}, {"running", 3}, {"running", 3}}
	for i, w := range want {
		p := getPool(t, s, cid, id)
		if p["status"] != w.status || num(p, "current_nodes") != w.cur {
			t.Fatalf("read %d after PUT = %v/%d, want %s/%d", i+1, p["status"], num(p, "current_nodes"), w.status, w.cur)
		}
	}
	// scaling down reads scaling-down; the lag knob reshapes the walk
	s.SetNodePoolScaleLag(2, 2)
	putPool(t, s, cid, id, `{"min_nodes":1,"desired_nodes":1,"max_nodes":1}`)
	var got []string
	for i := 0; i < 6; i++ {
		got = append(got, getPool(t, s, cid, id)["status"].(string))
	}
	if strings.Join(got, ",") != "running,running,scaling-down,scaling-down,running,running" {
		t.Fatalf("with lag (2,2) statuses = %v", got)
	}
	if p := getPool(t, s, cid, id); num(p, "current_nodes") != 1 {
		t.Fatalf("converged pool = %v, want current 1", p)
	}
}

func TestPoolPutOnAnAutoscalingPoolIsInertForDesiredAndNeverScales(t *testing.T) {
	s, cid := newPoolFake(t)
	id := createPool(t, s, cid, `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"min_vcpus":2,"max_vcpus":8,"min_memory_gb":4,"max_memory_gb":16}`)["id"].(string)
	getPool(t, s, cid, id)
	putPool(t, s, cid, id, `{"min_nodes":2,"desired_nodes":2,"max_nodes":2}`)
	for i := 0; i < 5; i++ {
		p := getPool(t, s, cid, id)
		if p["status"] != "running" || num(p, "current_nodes") != 1 || num(p, "desired_nodes") != 2 {
			t.Fatalf("read %d = %v, want running, current 1 (inert), desired 2 stored", i, p)
		}
	}
	s.SetNodePoolCurrentNodes("*", 4)
	if p := getPool(t, s, cid, id); num(p, "current_nodes") != 4 || num(p, "desired_nodes") != 2 || p["status"] != "running" {
		t.Fatalf("after the autoscaler moved it: %v", p)
	}
}

func TestPoolPutIsAPartialMergeWithNullMeaningKeep(t *testing.T) {
	s, cid := newPoolFake(t)
	id := createPool(t, s, cid, `{"name":"a","vcpus":2,"memory_gb":4,"disk_gb":20,"drain_timeout_seconds":120,"scale_down_delay_seconds":300}`)["id"].(string)
	code, p := putPool(t, s, cid, id, `{"drain_timeout_seconds":null,"scale_down_utilization_percent":50}`)
	if code != 200 || num(p, "drain_timeout_seconds") != 120 || num(p, "scale_down_delay_seconds") != 300 || num(p, "scale_down_utilization_percent") != 50 || p["name"] != "a" {
		t.Fatalf("PUT = %d %v: null must keep the stored knob, omitted fields stay", code, p)
	}
	code, p = putPool(t, s, cid, id, `{"min_vcpus":2,"max_vcpus":8,"min_memory_gb":4,"max_memory_gb":16}`)
	if code != 200 || num(p, "max_vcpus") != 8 {
		t.Fatalf("budget PUT = %d %v", code, p)
	}
	code, p = putPool(t, s, cid, id, `{"min_vcpus":0,"max_vcpus":0,"min_memory_gb":0,"max_memory_gb":0}`)
	if code != 200 || num(p, "max_vcpus") != 0 || num(p, "min_memory_gb") != 0 {
		t.Fatalf("explicit zeros = %d %v, want them written (back to a fixed pool)", code, p)
	}
	if updated := getPool(t, s, cid, id)["updated_at"]; updated == createPool(t, s, cid, `{"name":"x","vcpus":2,"memory_gb":4,"disk_gb":20}`)["updated_at"] {
		t.Fatalf("updated_at did not move with the PUTs")
	}
}

// aether-api validates a desired-only PUT against the STORED min/max but the SQL
// re-pins min and max to desired: two different wrong answers (contract §3).
func TestPoolPutOfDesiredAloneReproducesTheAPIsValidationVersusWriteMismatch(t *testing.T) {
	s, cid := newPoolFake(t)
	id := createPool(t, s, cid, fixedPool)["id"].(string) // 1/1/1
	code, m := putPool(t, s, cid, id, `{"desired_nodes":3}`)
	if code != 422 || errCode(m) != "validation_error" {
		t.Fatalf("desired-only on 1/1/1 = %d %v, want 422 (validated against stored max 1)", code, m)
	}
	id2 := createPool(t, s, cid, `{"name":"b","vcpus":2,"memory_gb":4,"disk_gb":20,"min_nodes":1,"desired_nodes":1,"max_nodes":5}`)["id"].(string)
	code, p := putPool(t, s, cid, id2, `{"desired_nodes":3}`)
	if code != 200 || num(p, "min_nodes") != 3 || num(p, "desired_nodes") != 3 || num(p, "max_nodes") != 3 {
		t.Fatalf("desired-only on 1/1/5 = %d %v, want 200 persisting 3/3/3", code, p)
	}
	id3 := createPool(t, s, cid, `{"name":"c","vcpus":2,"memory_gb":4,"disk_gb":20}`)["id"].(string)
	code, p = putPool(t, s, cid, id3, `{"min_nodes":1,"desired_nodes":3,"max_nodes":5}`)
	if code != 200 || num(p, "min_nodes") != 1 || num(p, "desired_nodes") != 3 || num(p, "max_nodes") != 5 {
		t.Fatalf("all three = %d %v", code, p)
	}
}

func TestPoolPutRefusesLabelsTaintsAndUnknownKeysWith400(t *testing.T) {
	s, cid := newPoolFake(t)
	id := createPool(t, s, cid, fixedPool)["id"].(string)
	for _, body := range []string{`{"node_labels":{"a":"b"}}`, `{"node_taints":[]}`, `{"bogus":1}`, `{not json`} {
		code, m := putPool(t, s, cid, id, body)
		if code != 400 || errCode(m) != "bad_request" || m["error"].(map[string]any)["message"] != "Invalid JSON body" {
			t.Fatalf("PUT %s = %d %v, want 400 bad_request 'Invalid JSON body'", body, code, m)
		}
	}
}

func TestPoolPutValidationRenameCollisionAndUnvalidatedDiskGB(t *testing.T) {
	s, cid := newPoolFake(t)
	id := createPool(t, s, cid, fixedPool)["id"].(string)
	createPool(t, s, cid, `{"name":"other","vcpus":2,"memory_gb":4,"disk_gb":20}`)
	if code, m := putPool(t, s, cid, id, `{"drain_timeout_seconds":4000}`); code != 422 {
		t.Fatalf("drain 4000 = %d %v, want 422", code, m)
	}
	if code, m := putPool(t, s, cid, id, `{"max_vcpus":12}`); code != 422 {
		t.Fatalf("a budget with max_vcpus alone (max_memory_gb 0 is below memory_gb) = %d %v, want 422", code, m)
	}
	if code, m := putPool(t, s, cid, id, `{"name":"other"}`); code != 409 || errCode(m) != "conflict" {
		t.Fatalf("rename collision = %d %v, want 409 conflict", code, m)
	}
	if code, p := putPool(t, s, cid, id, `{"name":"renamed","vcpus":4}`); code != 200 || p["name"] != "renamed" || num(p, "vcpus") != 4 {
		t.Fatalf("rename + resize = %d %v: the API accepts both in place", code, p)
	}
	if code, m := putPool(t, s, cid, id, `{"disk_gb":5000}`); code != 500 || errCode(m) != "internal_error" {
		t.Fatalf("disk_gb 5000 = %d %v, want the API's 500 internal_error (not validated on update)", code, m)
	}
	if code, m := putPool(t, s, cid, uuid.NewString(), `{"vcpus":4}`); code != 404 {
		t.Fatalf("PUT unknown pool = %d %v", code, m)
	}
}

func TestPoolDeleteIs202ThenGetReadsDeletingUntilTheSweepThen404AndSecondDeleteIs404(t *testing.T) {
	s, cid := newPoolFake(t)
	s.NodePoolDeleteBecomes404After("*", 2)
	id := createPool(t, s, cid, fixedPool)["id"].(string)
	resp, b := call(t, s, http.MethodDelete, poolsPath(cid)+"/"+id, "")
	if resp.StatusCode != 202 || decode(t, b)["status"] != "deleting" {
		t.Fatalf("DELETE = %d %s, want 202 {status: deleting}", resp.StatusCode, b)
	}
	if resp, b := call(t, s, http.MethodDelete, poolsPath(cid)+"/"+id, ""); resp.StatusCode != 404 {
		t.Fatalf("second DELETE = %d %s, want 404", resp.StatusCode, b)
	}
	if code, _ := putPool(t, s, cid, id, `{"drain_timeout_seconds":60}`); code != 200 {
		t.Fatalf("PUT on a deleting pool = %d, want 200", code)
	}
	for i := 0; i < 2; i++ {
		if p := getPool(t, s, cid, id); p["status"] != "deleting" {
			t.Fatalf("read %d = %v, want 200 deleting", i+1, p["status"])
		}
	}
	if resp, _ := call(t, s, http.MethodGet, poolsPath(cid)+"/"+id, ""); resp.StatusCode != 404 {
		t.Fatalf("read after the sweep = %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodDelete, poolsPath(cid)+"/"+id, ""); resp.StatusCode != 404 {
		t.Fatalf("DELETE after the sweep = %d, want 404", resp.StatusCode)
	}
}

func TestPoolDeleteDefaultsTo404OnTheFirstGetAndIsNeverRefused(t *testing.T) {
	s, cid := newPoolFake(t) // cluster still provisioning; the only pool; untainted
	id := createPool(t, s, cid, fixedPool)["id"].(string)
	if resp, _ := call(t, s, http.MethodDelete, poolsPath(cid)+"/"+id, ""); resp.StatusCode != 202 {
		t.Fatalf("delete of the last pool = %d, want 202", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodGet, poolsPath(cid)+"/"+id, ""); resp.StatusCode != 404 {
		t.Fatalf("first GET after DELETE = %d, want 404 by default", resp.StatusCode)
	}
}

func TestNodePoolGetFailsAfterDeleteAnswersTheCodeAndCanBeCleared(t *testing.T) {
	s, cid := newPoolFake(t)
	s.NodePoolDeleteBecomes404After("*", 5)
	id := createPool(t, s, cid, fixedPool)["id"].(string)
	call(t, s, http.MethodDelete, poolsPath(cid)+"/"+id, "")
	s.NodePoolGetFailsAfterDelete("*", 403, "token revoked")
	resp, b := call(t, s, http.MethodGet, poolsPath(cid)+"/"+id, "")
	e := decode(t, b)["error"].(map[string]any)
	if resp.StatusCode != 403 || e["code"] != "forbidden" || e["message"] != "token revoked" {
		t.Fatalf("GET = %d %s", resp.StatusCode, b)
	}
	s.NodePoolGetFailsAfterDelete("*", 0, "")
	if resp, _ := call(t, s, http.MethodGet, poolsPath(cid)+"/"+id, ""); resp.StatusCode != 200 {
		t.Fatalf("after clearing = %d, want 200 deleting", resp.StatusCode)
	}
}

func TestRemoveNodePoolIsOutOfBandAndUnrecordedAndNodePoolIDsListOldestFirst(t *testing.T) {
	s, cid := newPoolFake(t)
	a := createPool(t, s, cid, fixedPool)["id"].(string)
	b := createPool(t, s, cid, `{"name":"b","vcpus":2,"memory_gb":4,"disk_gb":20}`)["id"].(string)
	if ids := s.NodePoolIDs(cid); len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Fatalf("NodePoolIDs = %v, want [%s %s]", ids, a, b)
	}
	before := len(s.Requests())
	s.RemoveNodePool(a)
	if len(s.Requests()) != before {
		t.Fatal("RemoveNodePool must not record a request")
	}
	if resp, _ := call(t, s, http.MethodGet, poolsPath(cid)+"/"+a, ""); resp.StatusCode != 404 {
		t.Fatalf("removed pool = %d, want 404", resp.StatusCode)
	}
	s.RemoveNodePool("*")
	if ids := s.NodePoolIDs(cid); len(ids) != 0 {
		t.Fatalf("after removing all: %v", ids)
	}
}

func TestNodePoolCreateRespondsWithNonJSONAnswers202TextPlainButCreatesThePool(t *testing.T) {
	s, cid := newPoolFake(t)
	s.NodePoolCreateRespondsWithNonJSON(true)
	resp, b := call(t, s, http.MethodPost, poolsPath(cid), fixedPool)
	if resp.StatusCode != 202 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || bytes.HasPrefix(b, []byte("{")) {
		t.Fatalf("POST = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	if len(s.NodePoolIDs(cid)) != 1 {
		t.Fatal("the pool must still be created")
	}
}

func TestPoolListIsOldestFirstNeverNullAndPaginatesByCursor(t *testing.T) {
	s, cid := newPoolFake(t)
	resp, b := call(t, s, http.MethodGet, poolsPath(cid), "")
	m := decode(t, b)
	if items, ok := m["items"].([]any); resp.StatusCode != 200 || !ok || len(items) != 0 {
		t.Fatalf("empty list = %d %s, want items: []", resp.StatusCode, b)
	}
	var ids []string
	for _, n := range []string{"a", "b", "c"} {
		ids = append(ids, createPool(t, s, cid, `{"name":"`+n+`","vcpus":2,"memory_gb":4,"disk_gb":20}`)["id"].(string))
	}
	_, b = call(t, s, http.MethodGet, poolsPath(cid)+"?limit=2", "")
	m = decode(t, b)
	items := m["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["id"] != ids[0] || items[1].(map[string]any)["id"] != ids[1] || m["next_cursor"] != ids[1] {
		t.Fatalf("page 1 = %s, want [a b] with next_cursor = b", b)
	}
	_, b = call(t, s, http.MethodGet, poolsPath(cid)+"?limit=2&cursor="+ids[1], "")
	m = decode(t, b)
	items = m["items"].([]any)
	if _, has := m["next_cursor"]; len(items) != 1 || items[0].(map[string]any)["id"] != ids[2] || has {
		t.Fatalf("page 2 = %s, want [c] and no next_cursor", b)
	}
}

func TestPoolRoutesAreRecordedWithTheirBodies(t *testing.T) {
	s, cid := newPoolFake(t)
	id := createPool(t, s, cid, fixedPool)["id"].(string)
	putPool(t, s, cid, id, `{"drain_timeout_seconds":60}`)
	var post, put bool
	for _, r := range s.Requests() {
		if r.Method == http.MethodPost && r.Path == "/organizations/"+testOrg+"/clusters/"+cid+"/nodepools" && string(r.Body) == fixedPool {
			post = true
		}
		if r.Method == http.MethodPut && strings.HasSuffix(r.Path, "/nodepools/"+id) && string(r.Body) == `{"drain_timeout_seconds":60}` {
			put = true
		}
	}
	if !post || !put {
		t.Fatalf("recorded post=%v put=%v", post, put)
	}
}

func TestNodePoolDeleteRespondsNotFoundAnswers404ButTheRowIsGone(t *testing.T) {
	s, cid := newPoolFake(t)
	s.NodePoolDeleteRespondsNotFound(true)
	id := createPool(t, s, cid, fixedPool)["id"].(string)
	resp, b := call(t, s, http.MethodDelete, poolsPath(cid)+"/"+id, "")
	if resp.StatusCode != 404 || errCode(decode(t, b)) != "not_found" {
		t.Fatalf("DELETE = %d %s, want 404 not_found", resp.StatusCode, b)
	}
	if ids := s.NodePoolIDs(cid); len(ids) != 0 {
		t.Fatalf("the pool must be gone after the 404 DELETE, still have %v", ids)
	}
	s.NodePoolDeleteRespondsNotFound(false)
	id = createPool(t, s, cid, fixedPool)["id"].(string)
	if resp, _ := call(t, s, http.MethodDelete, poolsPath(cid)+"/"+id, ""); resp.StatusCode != 202 {
		t.Fatalf("after clearing the knob DELETE = %d, want 202", resp.StatusCode)
	}
}
