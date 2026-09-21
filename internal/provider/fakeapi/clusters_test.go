package fakeapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const testOrg = "11111111-1111-4111-8111-111111111111"

func newClusterFake(t *testing.T) *Server {
	t.Helper()
	s := New(t)
	s.AddOrg(testOrg, "Acme", "acme")
	return s
}

func call(t *testing.T, s *Server, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, s.URL()+"/api/v1"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %q: %v", b, err)
	}
	return m
}

const clustersPath = "/organizations/" + testOrg + "/clusters"

func create(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()
	resp, b := call(t, s, http.MethodPost, clustersPath, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST = %d %s, want 202", resp.StatusCode, b)
	}
	return decode(t, b)
}

func getStatus(t *testing.T, s *Server, id string) string {
	t.Helper()
	resp, b := call(t, s, http.MethodGet, clustersPath+"/"+id, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s, want 200", resp.StatusCode, b)
	}
	return decode(t, b)["status"].(string)
}

func TestClusterCreateAnswers202WithARealUUIDAndProvisioning(t *testing.T) {
	s := newClusterFake(t)
	c := create(t, s, `{"name":"prod","region":"eu-fi","k8s_version":"1.37"}`)
	if _, err := uuid.Parse(c["id"].(string)); err != nil {
		t.Fatalf("id %v is not a UUID: %v", c["id"], err)
	}
	if c["org_id"] != testOrg || c["name"] != "prod" || c["region"] != "eu-fi" || c["k8s_version"] != "1.37" {
		t.Fatalf("body = %v", c)
	}
	if c["status"] != "provisioning" {
		t.Fatalf("status = %v, want provisioning on the POST", c["status"])
	}
	if c["observed_kubernetes_version"] != "1.37.0" {
		t.Fatalf("observed = %v, want 1.37.0", c["observed_kubernetes_version"])
	}
	if c["public_endpoint"] != false {
		t.Fatalf("public_endpoint = %v, want false when omitted", c["public_endpoint"])
	}
	if _, has := c["plan_slug"]; has {
		t.Fatalf("plan_slug present without a plan: %v", c["plan_slug"])
	}
	id2 := create(t, s, `{"name":"b","region":"eu-fi","k8s_version":"1.37"}`)["id"]
	if id2 == c["id"] {
		t.Fatal("two clusters got the same id")
	}
}

func TestClusterCreateEchoesPlanAndPublicEndpointButNotAnEmptyPlan(t *testing.T) {
	s := newClusterFake(t)
	c := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37","plan":"starter","public_endpoint":true}`)
	if c["plan_slug"] != "starter" || c["public_endpoint"] != true {
		t.Fatalf("body = %v", c)
	}
	c = create(t, s, `{"name":"b","region":"r","k8s_version":"1.37","plan":""}`)
	if _, has := c["plan_slug"]; has {
		t.Fatalf("an empty plan means custom billing; plan_slug must stay absent, got %v", c["plan_slug"])
	}
}

func TestClusterCreateValidationErrorUsesTheEnvelopeWithRequestIDOnlyInTheHeader(t *testing.T) {
	s := newClusterFake(t)
	resp, b := call(t, s, http.MethodPost, clustersPath, `{"name":"x"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("no X-Request-ID header")
	}
	if bytes.Contains(b, []byte("request_id")) {
		t.Fatalf("request id leaked into the body: %s", b)
	}
	e := decode(t, b)["error"].(map[string]any)
	if e["code"] != "validation_error" || e["message"] == "" || len(e["details"].([]any)) != 2 {
		t.Fatalf("error = %v, want code, message and 2 details (region, k8s_version)", e)
	}
}

func TestClusterUnknownOrgAndUnknownClusterAre404(t *testing.T) {
	s := newClusterFake(t)
	if resp, _ := call(t, s, http.MethodGet, "/organizations/22222222-2222-4222-8222-222222222222/clusters", ""); resp.StatusCode != 404 {
		t.Fatalf("unknown org = %d", resp.StatusCode)
	}
	resp, b := call(t, s, http.MethodGet, clustersPath+"/"+uuid.NewString(), "")
	if resp.StatusCode != 404 || decode(t, b)["error"] == nil {
		t.Fatalf("unknown cluster = %d %s", resp.StatusCode, b)
	}
	if resp, _ := call(t, s, http.MethodGet, clustersPath+"/x/nothing", ""); resp.StatusCode != 404 {
		t.Fatalf("unknown route = %d", resp.StatusCode)
	}
}

func TestClusterListReturnsCreatedClustersOldestFirst(t *testing.T) {
	s := newClusterFake(t)
	a := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"]
	b := create(t, s, `{"name":"b","region":"r","k8s_version":"1.37"}`)["id"]
	resp, body := call(t, s, http.MethodGet, clustersPath, "")
	items := decode(t, body)["items"].([]any)
	if resp.StatusCode != 200 || len(items) != 2 || items[0].(map[string]any)["id"] != a || items[1].(map[string]any)["id"] != b {
		t.Fatalf("list = %d %s", resp.StatusCode, body)
	}
}

func TestStatusSequenceWalksAndLastRepeats(t *testing.T) {
	s := newClusterFake(t)
	s.StatusSequence("*", "provisioning", "degraded", "running")
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	var got []string
	for i := 0; i < 5; i++ {
		got = append(got, getStatus(t, s, id))
	}
	if strings.Join(got, ",") != "provisioning,degraded,running,running,running" {
		t.Fatalf("got %v", got)
	}
}

func TestStatusSequenceDefaultsToRunningAndASpecificIDBeatsWildcard(t *testing.T) {
	s := newClusterFake(t)
	a := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	b := create(t, s, `{"name":"b","region":"r","k8s_version":"1.37"}`)["id"].(string)
	if got := getStatus(t, s, a); got != "running" {
		t.Fatalf("default = %s", got)
	}
	s.StatusSequence("*", "failed")
	s.StatusSequence(b, "degraded")
	if ga, gb := getStatus(t, s, a), getStatus(t, s, b); ga != "failed" || gb != "degraded" {
		t.Fatalf("a=%s b=%s, want failed/degraded", ga, gb)
	}
}

func TestStatusSequenceSetAfterCreationRestartsTheWalk(t *testing.T) {
	s := newClusterFake(t)
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	getStatus(t, s, id)
	getStatus(t, s, id)
	s.StatusSequence("*", "provisioning", "running")
	if got := getStatus(t, s, id); got != "provisioning" {
		t.Fatalf("first read after setting the sequence = %s, want provisioning", got)
	}
}

func TestSetStatusMessageAppearsOnReads(t *testing.T) {
	s := newClusterFake(t)
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	_, b := call(t, s, http.MethodGet, clustersPath+"/"+id, "")
	if _, has := decode(t, b)["status_message"]; has {
		t.Fatal("status_message present before it was set")
	}
	s.SetStatusMessage("*", "no capacity in eu-fi")
	_, b = call(t, s, http.MethodGet, clustersPath+"/"+id, "")
	if decode(t, b)["status_message"] != "no capacity in eu-fi" {
		t.Fatalf("body = %s", b)
	}
}

func TestClusterPutRenamesAndRefusesAChangedPublicEndpoint(t *testing.T) {
	s := newClusterFake(t)
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	resp, b := call(t, s, http.MethodPut, clustersPath+"/"+id, `{"name":"renamed","public_endpoint":false}`)
	if resp.StatusCode != 200 || decode(t, b)["name"] != "renamed" {
		t.Fatalf("PUT = %d %s", resp.StatusCode, b)
	}
	resp, b = call(t, s, http.MethodPut, clustersPath+"/"+id, `{"public_endpoint":true}`)
	if resp.StatusCode != 422 {
		t.Fatalf("PUT public_endpoint change = %d %s, want 422", resp.StatusCode, b)
	}
	d := decode(t, b)["error"].(map[string]any)["details"].([]any)[0].(map[string]any)
	if d["field"] != "public_endpoint" || d["code"] != "immutable" {
		t.Fatalf("details = %v", d)
	}
	_, b = call(t, s, http.MethodGet, clustersPath+"/"+id, "")
	if decode(t, b)["name"] != "renamed" {
		t.Fatalf("rename not persisted: %s", b)
	}
	if resp, _ := call(t, s, http.MethodPut, clustersPath+"/"+uuid.NewString(), `{"name":"x"}`); resp.StatusCode != 404 {
		t.Fatalf("PUT unknown = %d", resp.StatusCode)
	}
}

func TestClusterUpgradeAnswers202UpdatesVersionsAndRestartsTheWalk(t *testing.T) {
	s := newClusterFake(t)
	s.StatusSequence("*", "running")
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.36"}`)["id"].(string)
	getStatus(t, s, id)
	s.StatusSequence("*", "upgrading", "running")
	getStatus(t, s, id)
	getStatus(t, s, id)
	resp, b := call(t, s, http.MethodPost, clustersPath+"/"+id+"/upgrade", `{"target_version":"1.37"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("upgrade = %d %s", resp.StatusCode, b)
	}
	cl := decode(t, b)["cluster"].(map[string]any)
	if cl["k8s_version"] != "1.37" || cl["observed_kubernetes_version"] != "1.37.0" {
		t.Fatalf("cluster = %v", cl)
	}
	if got := getStatus(t, s, id); got != "upgrading" {
		t.Fatalf("first read after the upgrade = %s, want upgrading (walk restarted)", got)
	}
	if got := getStatus(t, s, id); got != "running" {
		t.Fatalf("second read = %s, want running", got)
	}
	if resp, _ := call(t, s, http.MethodPost, clustersPath+"/"+id+"/upgrade", `{}`); resp.StatusCode != 422 {
		t.Fatalf("upgrade without target_version = %d, want 422", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodPost, clustersPath+"/"+uuid.NewString()+"/upgrade", `{"target_version":"1.37"}`); resp.StatusCode != 404 {
		t.Fatalf("upgrade unknown = %d, want 404", resp.StatusCode)
	}
}

func TestClusterUpgradeKeepsAnExplicitPatchAsObserved(t *testing.T) {
	s := newClusterFake(t)
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.36"}`)["id"].(string)
	_, b := call(t, s, http.MethodPost, clustersPath+"/"+id+"/upgrade", `{"target_version":"1.37.2"}`)
	if got := decode(t, b)["cluster"].(map[string]any)["observed_kubernetes_version"]; got != "1.37.2" {
		t.Fatalf("observed = %v, want 1.37.2", got)
	}
}

func TestDeleteAnswers202ThenTheFirstGetIs404ByDefault(t *testing.T) {
	s := newClusterFake(t)
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	resp, b := call(t, s, http.MethodDelete, clustersPath+"/"+id, "")
	if resp.StatusCode != 202 || decode(t, b)["status"] != "deleting" {
		t.Fatalf("DELETE = %d %s", resp.StatusCode, b)
	}
	if resp, _ := call(t, s, http.MethodGet, clustersPath+"/"+id, ""); resp.StatusCode != 404 {
		t.Fatalf("GET after delete = %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodDelete, clustersPath+"/"+id, ""); resp.StatusCode != 404 {
		t.Fatalf("second DELETE = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteBecomes404AfterNGets(t *testing.T) {
	s := newClusterFake(t)
	s.DeleteBecomes404After("*", 2)
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	call(t, s, http.MethodDelete, clustersPath+"/"+id, "")
	for i := 0; i < 2; i++ {
		if got := getStatus(t, s, id); got != "deleting" {
			t.Fatalf("read %d = %s, want deleting", i, got)
		}
	}
	if resp, _ := call(t, s, http.MethodGet, clustersPath+"/"+id, ""); resp.StatusCode != 404 {
		t.Fatalf("third read = %d, want 404", resp.StatusCode)
	}
	if len(s.ClusterIDs()) != 0 {
		t.Fatalf("cluster still held: %v", s.ClusterIDs())
	}
}

func TestGetFailsAfterDeleteAnswersTheCodeAndCanBeCleared(t *testing.T) {
	s := newClusterFake(t)
	id := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	s.GetFailsAfterDelete("*", http.StatusForbidden, "token revoked mid-delete")
	if got := getStatus(t, s, id); got != "running" {
		t.Fatalf("before DELETE the knob must not apply, got %s", got)
	}
	call(t, s, http.MethodDelete, clustersPath+"/"+id, "")
	resp, b := call(t, s, http.MethodGet, clustersPath+"/"+id, "")
	if resp.StatusCode != 403 || decode(t, b)["error"].(map[string]any)["message"] != "token revoked mid-delete" {
		t.Fatalf("GET = %d %s", resp.StatusCode, b)
	}
	s.GetFailsAfterDelete("*", 0, "")
	if resp, _ := call(t, s, http.MethodGet, clustersPath+"/"+id, ""); resp.StatusCode != 404 {
		t.Fatalf("after clearing the knob = %d, want 404 (delete completes)", resp.StatusCode)
	}
}

func TestRemoveClusterIsOutOfBandAndUnrecorded(t *testing.T) {
	s := newClusterFake(t)
	a := create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)["id"].(string)
	b := create(t, s, `{"name":"b","region":"r","k8s_version":"1.37"}`)["id"].(string)
	before := len(s.Requests())
	s.RemoveCluster(a)
	if len(s.Requests()) != before {
		t.Fatal("RemoveCluster must not be a recorded request")
	}
	if resp, _ := call(t, s, http.MethodGet, clustersPath+"/"+a, ""); resp.StatusCode != 404 {
		t.Fatalf("removed cluster = %d", resp.StatusCode)
	}
	if got := getStatus(t, s, b); got != "running" {
		t.Fatalf("other cluster affected: %s", got)
	}
	s.RemoveCluster("*")
	if len(s.ClusterIDs()) != 0 {
		t.Fatalf("ids = %v", s.ClusterIDs())
	}
}

func TestCreateRespondsWithNonJSONAnswers202TextPlain(t *testing.T) {
	s := newClusterFake(t)
	s.CreateRespondsWithNonJSON(true)
	resp, b := call(t, s, http.MethodPost, clustersPath, `{"name":"a","region":"r","k8s_version":"1.37"}`)
	if resp.StatusCode != 202 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || json.Valid(b) {
		t.Fatalf("got %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	if len(s.ClusterIDs()) != 1 {
		t.Fatal("the cluster must still be created in the fake")
	}
	s.CreateRespondsWithNonJSON(false)
	create(t, s, `{"name":"b","region":"r","k8s_version":"1.37"}`)
}

func TestClusterRoutesAreRecordedAndNeedTheToken(t *testing.T) {
	s := newClusterFake(t)
	create(t, s, `{"name":"a","region":"r","k8s_version":"1.37"}`)
	reqs := s.Requests()
	last := reqs[len(reqs)-1]
	if last.Method != http.MethodPost || last.Path != clustersPath || !strings.Contains(string(last.Body), `"name":"a"`) {
		t.Fatalf("recorded %+v", last)
	}
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/api/v1"+clustersPath, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no token = %d", resp.StatusCode)
	}
}
