package fakeapi

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// newNetworkFake returns a fake with one org and one cluster (whatever
// status the cluster POST leaves it in - the network route has no
// cluster-status precondition, contract §8).
func newNetworkFake(t *testing.T) (*Server, string) {
	t.Helper()
	s := newClusterFake(t)
	cid := create(t, s, `{"name":"c","region":"r","k8s_version":"1.37"}`)["id"].(string)
	return s, cid
}

func networkPath(cid string) string { return clustersPath + "/" + cid + "/network" }

func getNetwork(t *testing.T, s *Server, cid string) (*http.Response, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodGet, networkPath(cid), "")
	return resp, decode(t, b)
}

func putNetwork(t *testing.T, s *Server, cid, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodPut, networkPath(cid), body)
	return resp, decode(t, b)
}

// ---------------------------------------------------------------- §2

// Fails if the fake ever adds an api_server_rate_limit key, uses a non-zero
// or "now" updated_at, returns null/missing authorized_cidrs, or answers
// anything but 200 before any PUT has happened.
func TestNetworkGetBeforeAnyPUTIsByteExact(t *testing.T) {
	s, cid := newNetworkFake(t)
	resp, b := call(t, s, http.MethodGet, networkPath(cid), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s, want 200 (never 404 for an unset policy)", resp.StatusCode, b)
	}
	// encoding/json sorts map keys alphabetically, so this exact byte order is
	// deterministic regardless of the map literal's construction order in the
	// implementation - a genuine byte-for-byte assertion, not a lucky match.
	want := `{"authorized_cidrs":[],"cluster_id":"` + cid + `","updated_at":"0001-01-01T00:00:00Z"}`
	if string(b) != want {
		t.Fatalf("body = %s,\nwant exactly %s\n(no api_server_rate_limit key, Go's zero time, cluster_id from the URL)", b, want)
	}
}

// ---------------------------------------------------------------- §3

// Fails if the fake normalises/reorders the CIDR list, drops the rate limit,
// or fails to bump updated_at on a byte-identical re-PUT.
func TestNetworkPutIsFullReplaceAndBumpsUpdatedAtOnAnIdenticalRePUT(t *testing.T) {
	s, cid := newNetworkFake(t)
	body := `{"authorized_cidrs":["10.0.0.0/24","192.168.1.1/32"],"api_server_rate_limit":{"max_connections_per_second":100,"burst":200}}`
	resp, m := putNetwork(t, s, cid, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %v, want 200", resp.StatusCode, m)
	}
	cidrs := m["authorized_cidrs"].([]any)
	if len(cidrs) != 2 || cidrs[0] != "10.0.0.0/24" || cidrs[1] != "192.168.1.1/32" {
		t.Fatalf("authorized_cidrs = %v, want the list echoed verbatim in order (no normalisation, no dedupe)", m["authorized_cidrs"])
	}
	rl, ok := m["api_server_rate_limit"].(map[string]any)
	if !ok || num(rl, "max_connections_per_second") != 100 || num(rl, "burst") != 200 {
		t.Fatalf("api_server_rate_limit = %v, want it echoed back", m["api_server_rate_limit"])
	}
	updated1, _ := m["updated_at"].(string)
	if updated1 == "" || updated1 == "0001-01-01T00:00:00Z" {
		t.Fatalf("updated_at = %q, want a real, non-zero timestamp after the first PUT", updated1)
	}

	// A byte-identical re-PUT still moves updated_at (§3: "even an identical one").
	resp2, m2 := putNetwork(t, s, cid, body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second PUT = %d %v, want 200", resp2.StatusCode, m2)
	}
	updated2, _ := m2["updated_at"].(string)
	if updated2 == updated1 {
		t.Fatalf("updated_at stayed %q across a second, byte-identical PUT; it must move on every PUT", updated2)
	}
}

// Fails if omitting api_server_rate_limit on a later PUT keeps the
// previously-stored value instead of clearing it back to absent.
func TestNetworkOmittedRateLimitClearsAPreviouslyStoredOne(t *testing.T) {
	s, cid := newNetworkFake(t)
	putNetwork(t, s, cid, `{"authorized_cidrs":[],"api_server_rate_limit":{"max_connections_per_second":10,"burst":20}}`)
	resp, m := putNetwork(t, s, cid, `{"authorized_cidrs":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %v, want 200", resp.StatusCode, m)
	}
	if v, has := m["api_server_rate_limit"]; has {
		t.Fatalf("api_server_rate_limit = %v present in the PUT response, want absent - omission RESETS it, it does not keep the stored value (§3)", v)
	}
	// and the state really persisted cleared, not just this one response
	_, g := getNetwork(t, s, cid)
	if v, has := g["api_server_rate_limit"]; has {
		t.Fatalf("a later GET still carries api_server_rate_limit = %v, want it to stay cleared", v)
	}
}

// Fails if an EXPLICIT null is treated differently from omission (both must
// clear, per §3).
func TestNetworkExplicitNullRateLimitAlsoClears(t *testing.T) {
	s, cid := newNetworkFake(t)
	putNetwork(t, s, cid, `{"authorized_cidrs":[],"api_server_rate_limit":{"max_connections_per_second":10,"burst":20}}`)
	resp, m := putNetwork(t, s, cid, `{"authorized_cidrs":[],"api_server_rate_limit":null}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %v, want 200", resp.StatusCode, m)
	}
	if v, has := m["api_server_rate_limit"]; has {
		t.Fatalf("api_server_rate_limit = %v present after an explicit null, want absent", v)
	}
}

// ---------------------------------------------------------------- §4

// Fails if any of these detail shapes drift from the exact field/code/message
// the real API returns, or if the fake answers something other than 422.
func TestNetworkPutValidationIs422WithExactFieldCodeAndMessage(t *testing.T) {
	cases := []struct {
		name, body, field, code, message string
	}{
		{"bare address at index 0", `{"authorized_cidrs":["10.0.0.1"]}`,
			"authorized_cidrs", "invalid_cidr", "Invalid CIDR at index 0: 10.0.0.1"},
		{"bad entry at index 1", `{"authorized_cidrs":["10.0.0.0/24","not-a-cidr"]}`,
			"authorized_cidrs", "invalid_cidr", "Invalid CIDR at index 1: not-a-cidr"},
		{"zero rate", `{"authorized_cidrs":[],"api_server_rate_limit":{"max_connections_per_second":0,"burst":10}}`,
			"api_server_rate_limit", "invalid", "max_connections_per_second and burst must both be greater than 0"},
		{"negative burst", `{"authorized_cidrs":[],"api_server_rate_limit":{"max_connections_per_second":10,"burst":-1}}`,
			"api_server_rate_limit", "invalid", "max_connections_per_second and burst must both be greater than 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, cid := newNetworkFake(t)
			resp, b := call(t, s, http.MethodPut, networkPath(cid), c.body)
			m := decode(t, b)
			if resp.StatusCode != http.StatusUnprocessableEntity || errCode(m) != "validation_error" {
				t.Fatalf("PUT %s = %d %s, want 422 validation_error", c.body, resp.StatusCode, b)
			}
			if resp.Header.Get("X-Request-ID") == "" {
				t.Fatalf("no X-Request-ID response header on a 422")
			}
			details, _ := m["error"].(map[string]any)["details"].([]any)
			found := false
			for _, d := range details {
				dm := d.(map[string]any)
				if dm["field"] == c.field && dm["code"] == c.code && dm["message"] == c.message {
					found = true
				}
			}
			if !found {
				t.Fatalf("details %v do not contain {field:%q code:%q message:%q}", details, c.field, c.code, c.message)
			}
		})
	}
}

// The API accepts what the platform's CRD later refuses (§4/§7): IPv6,
// duplicates, host bits and 0.0.0.0/0 must all be a plain 200, never a 422.
// Fails if the fake starts rejecting any of these (a v4-only check, a dedupe,
// or host-bit masking creeping into the fake would all trip this).
func TestNetworkPutAcceptsIPv6DuplicatesHostBitsAndZeroSlashZero(t *testing.T) {
	s, cid := newNetworkFake(t)
	body := `{"authorized_cidrs":["2001:db8::/32","10.0.0.1/8","10.0.0.1/8","0.0.0.0/0"]}`
	resp, m := putNetwork(t, s, cid, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT %s = %d %v, want 200 (the API validates only with net.ParseCIDR, no v4-only/dedupe/host-bit rule)", body, resp.StatusCode, m)
	}
	cidrs := m["authorized_cidrs"].([]any)
	if len(cidrs) != 4 {
		t.Fatalf("authorized_cidrs = %v, want all 4 entries kept including the duplicate", cidrs)
	}
}

// ---------------------------------------------------------------- §3/§4: 500s

// Missing or null authorized_cidrs is the NOT NULL column violation: a 500,
// never a 422 and never silently accepted. Fails if the fake starts treating
// either shape as legal or as a validation error instead.
func TestNetworkPutWithMissingOrNullAuthorizedCidrsIs500(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"authorized_cidrs":null}`,
		`{"api_server_rate_limit":{"max_connections_per_second":1,"burst":1}}`,
	} {
		s, cid := newNetworkFake(t)
		resp, b := call(t, s, http.MethodPut, networkPath(cid), body)
		m := decode(t, b)
		if resp.StatusCode != http.StatusInternalServerError || errCode(m) != "internal_error" {
			t.Fatalf("PUT %s = %d %s, want 500 internal_error (the NOT NULL column, never 422 and never accepted)", body, resp.StatusCode, b)
		}
	}
}

// A rate-limit value above int32 passes the >0 validator and fails only at
// the store step: 500, not 422. Fails if the fake instead accepts it, or
// rejects it with a 422.
func TestNetworkPutWithRateLimitAboveInt32Is500NotA422(t *testing.T) {
	for _, body := range []string{
		`{"authorized_cidrs":[],"api_server_rate_limit":{"max_connections_per_second":2147483648,"burst":10}}`,
		`{"authorized_cidrs":[],"api_server_rate_limit":{"max_connections_per_second":10,"burst":2147483648}}`,
	} {
		s, cid := newNetworkFake(t)
		resp, b := call(t, s, http.MethodPut, networkPath(cid), body)
		m := decode(t, b)
		if resp.StatusCode != http.StatusInternalServerError || errCode(m) != "internal_error" {
			t.Fatalf("PUT %s = %d %s, want 500 internal_error (int32 overflow at store time, not 422)", body, resp.StatusCode, b)
		}
	}
}

// ---------------------------------------------------------------- §1: 400

// Fails if the fake accepts (or mis-codes) an unknown top-level key instead
// of the strict-decode 400 every other write route uses.
func TestNetworkPutWithUnknownKeyIs400(t *testing.T) {
	s, cid := newNetworkFake(t)
	resp, b := call(t, s, http.MethodPut, networkPath(cid), `{"authorized_cidrs":[],"bogus":1}`)
	m := decode(t, b)
	if resp.StatusCode != http.StatusBadRequest || errCode(m) != "bad_request" {
		t.Fatalf("PUT with an unknown key = %d %s, want 400 bad_request", resp.StatusCode, b)
	}
}

// ---------------------------------------------------------------- §8

// Fails if the route stops keying off the existing cluster store (e.g. a
// hand-rolled cluster-existence check that forgets the org filter, or one
// that 200s an unknown cluster instead of 404ing it).
func TestNetworkRoutes404ForUnknownOrgOrCluster(t *testing.T) {
	s, cid := newNetworkFake(t)
	otherOrg := "22222222-2222-4222-8222-222222222222"
	unknownCluster := uuid.NewString()
	for _, path := range []string{
		"/organizations/" + otherOrg + "/clusters/" + cid + "/network",
		clustersPath + "/" + unknownCluster + "/network",
	} {
		resp, b := call(t, s, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusNotFound || errCode(decode(t, b)) != "not_found" {
			t.Fatalf("GET %s = %d %s, want 404 not_found", path, resp.StatusCode, b)
		}
		resp2, b2 := call(t, s, http.MethodPut, path, `{"authorized_cidrs":[]}`)
		if resp2.StatusCode != http.StatusNotFound || errCode(decode(t, b2)) != "not_found" {
			t.Fatalf("PUT %s = %d %s, want 404 not_found", path, resp2.StatusCode, b2)
		}
	}
}

// A cluster mid-deletion (DELETE issued, row not yet swept) still answers
// 200 to both GET and PUT of its network policy. Fails if the route instead
// 404s or otherwise special-cases a "deleting" cluster.
func TestNetworkGetAndPutStillAnswer200OnADeletingCluster(t *testing.T) {
	s, cid := newNetworkFake(t)
	putNetwork(t, s, cid, `{"authorized_cidrs":["10.0.0.0/24"]}`)
	if resp, b := call(t, s, http.MethodDelete, clustersPath+"/"+cid, ""); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("DELETE cluster = %d %s, want 202", resp.StatusCode, b)
	}
	resp, m := getNetwork(t, s, cid)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET network on a deleting cluster = %d %v, want 200 (§8)", resp.StatusCode, m)
	}
	resp2, m2 := putNetwork(t, s, cid, `{"authorized_cidrs":[]}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("PUT network on a deleting cluster = %d %v, want 200 (§8)", resp2.StatusCode, m2)
	}
}
