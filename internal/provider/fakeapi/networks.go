package fakeapi

// Cluster-network routes of the fake, reproducing what aether-api DOES
// (measured at aether-api 8e9045a; see the SDD task-8 contract,
// .superpowers/sdd/2026-09-17-terraform-provider-aether/task-8-measured-contract.md),
// NOT what its OpenAPI spec says:
//
//	GET  /organizations/{org}/clusters/{cid}/network   200 ClusterNetwork
//	PUT  /organizations/{org}/clusters/{cid}/network   200 ClusterNetwork | 400 | 422 | 500
//
// There is no DELETE, POST or PATCH.
//
// Behaviours that matter to a Terraform provider and that the fake copies
// (contract section numbers below):
//   - a never-configured cluster's GET is 200, not 404, with EXACTLY
//     {"cluster_id":"<uuid>","authorized_cidrs":[],"updated_at":"0001-01-01T00:00:00Z"}
//     - no api_server_rate_limit key, the Go zero time (§2);
//   - PUT is a full replace: authorized_cidrs is echoed verbatim (content and
//     order, no normalisation - not even masking host bits), and
//     api_server_rate_limit omitted or explicit null CLEARS the stored value
//     (absent on the next read - omission never keeps it); updated_at moves
//     on EVERY PUT, including a byte-identical re-PUT (§3);
//   - a missing or null authorized_cidrs is a 500 internal_error (the
//     NOT NULL DB column reached with a nil slice), never a 422 and never
//     silently accepted (§3);
//   - an unknown JSON key is a 400 bad_request (§1);
//   - a CIDR net.ParseCIDR rejects, or a present rate limit with
//     max_connections_per_second <= 0 || burst <= 0, is a 422
//     validation_error with the exact field/code/message (§4). IPv6,
//     duplicate entries, host bits (10.0.0.1/8) and 0.0.0.0/0 are all
//     ACCEPTED - there is no dedupe, no v4-only check and no normalisation;
//   - a rate limit value above int32 (> 2147483647) passes that validator
//     and fails at the (fake) store step instead: 500, never 422 (§4);
//   - a cluster in "deleting" still answers 200 to GET and PUT (§8);
//   - keyed on the EXISTING fake cluster store: an unknown cluster, or one
//     in a different org, is a 404 (§8); this file adds no separate 403 -
//     the real API's org-mismatch 403 is a token/auth-middleware behaviour
//     the resource tests reproduce with a Handlers override, not a
//     network-route concern;
//   - no knob to force a raw 500 is needed: the fake's own 500s are all
//     reproduced from the request body/values themselves.

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"
)

var networkPathRE = regexp.MustCompile(`^/organizations/([^/]+)/clusters/([^/]+)/network$`)

// networkRateLimit is the decoded api_server_rate_limit shape. Fields are
// int64 (not the client's plain int) so a value that exceeds int32 - legal
// JSON, illegal for the platform's INTEGER columns - decodes cleanly instead
// of silently truncating, which is what lets the fake reproduce the real
// int32-overflow 500 (§4) instead of hiding it.
type networkRateLimit struct {
	MaxConnectionsPerSecond int64 `json:"max_connections_per_second"`
	Burst                   int64 `json:"burst"`
}

// fakeNetwork is one cluster's stored network-policy row. It exists only
// after the first successful PUT; before that, GET synthesises the §2 body
// without ever creating a row (mirrors the store's pgx.ErrNoRows path).
type fakeNetwork struct {
	authorizedCIDRs []string          // always non-nil once a row exists, even when empty
	rateLimit       *networkRateLimit // nil = platform default (never echoed)
	mutations       int               // bumps updated_at; every PUT increments it, identical or not
}

// networkStore is embedded by value in Server; its zero value is usable.
type networkStore struct {
	mu   sync.Mutex
	rows map[string]*fakeNetwork
}

func (ns *networkStore) init() {
	if ns.rows == nil {
		ns.rows = map[string]*fakeNetwork{}
	}
}

// networkEpoch anchors the fake's updated_at clock; only relative movement
// (a later PUT reads later) and non-zero-ness matter to a caller, never this
// absolute value.
var networkEpoch = time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)

func networkUpdatedAt(mutations int) string {
	return networkEpoch.Add(time.Duration(mutations) * time.Second).Format(time.RFC3339)
}

// zeroTime is Go's zero time.Time formatted as RFC3339 - what a never-set,
// non-pointer time.Time field marshals as. Not networkEpoch: the never-PUT
// row has no mutations to count from at all (§2).
const zeroTime = "0001-01-01T00:00:00Z"

// json renders the ClusterNetwork schema for a row that has been PUT at
// least once. Caller holds ns.mu.
func (ns *networkStore) json(cid string, row *fakeNetwork) map[string]any {
	m := map[string]any{
		"cluster_id":       cid,
		"authorized_cidrs": row.authorizedCIDRs,
		"updated_at":       networkUpdatedAt(row.mutations),
	}
	if row.rateLimit != nil {
		m["api_server_rate_limit"] = map[string]any{
			"max_connections_per_second": row.rateLimit.MaxConnectionsPerSecond,
			"burst":                      row.rateLimit.Burst,
		}
	}
	return m
}

// routeClusterNetwork serves the cluster-network routes; it reports false
// when path is not one. It relies on the EXISTING cluster store for
// existence/org checks, so an unknown or cross-org cluster answers 404
// exactly like every other per-cluster route.
func (s *Server) routeClusterNetwork(w http.ResponseWriter, r *http.Request, path string, body []byte) bool {
	m := networkPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	org, cid := m[1], m[2]
	if !s.orgKnown(org) || !s.clusterInOrg(org, cid) {
		apiError(w, http.StatusNotFound, "not_found", "Resource not found")
		return true
	}
	ns := &s.networks
	ns.mu.Lock()
	defer ns.mu.Unlock()
	ns.init()
	switch r.Method {
	case http.MethodGet:
		ns.get(w, cid)
	case http.MethodPut:
		ns.put(w, cid, body)
	default:
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
	}
	return true
}

// get answers the §2 synthetic default when the cluster has never been PUT,
// or the stored row otherwise. Never 404 for "no policy set" - only the
// cluster itself missing is a 404, handled by the caller before this runs.
func (ns *networkStore) get(w http.ResponseWriter, cid string) {
	row, ok := ns.rows[cid]
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"cluster_id":       cid,
			"authorized_cidrs": []string{},
			"updated_at":       zeroTime,
		})
		return
	}
	writeJSON(w, http.StatusOK, ns.json(cid, row))
}

// put is a full replace. Every validation and 500 branch mirrors §3/§4 of
// the measured contract; see the file-level comment for the summary.
func (ns *networkStore) put(w http.ResponseWriter, cid string, body []byte) {
	var req struct {
		AuthorizedCidrs    *[]string         `json:"authorized_cidrs"`
		ApiServerRateLimit *networkRateLimit `json:"api_server_rate_limit"`
	}
	if !decodeStrict(body, &req) {
		apiError(w, http.StatusBadRequest, "bad_request", "Invalid JSON body")
		return
	}
	// A nil pointer here covers BOTH "key absent" and "key present as null" -
	// encoding/json cannot tell them apart on a pointer field, and neither can
	// the real API (§3): both are the same NOT-NULL-column violation.
	if req.AuthorizedCidrs == nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "Failed to update cluster network")
		return
	}
	cidrs := *req.AuthorizedCidrs

	var details []map[string]any
	for i, c := range cidrs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			details = append(details, detail("authorized_cidrs", "invalid_cidr", fmt.Sprintf("Invalid CIDR at index %d: %s", i, c)))
		}
	}
	if rl := req.ApiServerRateLimit; rl != nil && (rl.MaxConnectionsPerSecond <= 0 || rl.Burst <= 0) {
		details = append(details, detail("api_server_rate_limit", "invalid", "max_connections_per_second and burst must both be greater than 0"))
	}
	if details != nil {
		validationError(w, details)
		return
	}
	// Only reached once both fields pass Validate (i.e. any rate limit present
	// is strictly positive): an int32-overflowing value is legal to Validate
	// and fails at the (fake) store step instead - 500, never 422 (§4).
	if rl := req.ApiServerRateLimit; rl != nil && (rl.MaxConnectionsPerSecond > math.MaxInt32 || rl.Burst > math.MaxInt32) {
		apiError(w, http.StatusInternalServerError, "internal_error", "Failed to update cluster network")
		return
	}

	row, ok := ns.rows[cid]
	if !ok {
		row = &fakeNetwork{}
		ns.rows[cid] = row
	}
	// A fresh non-nil slice, even for an empty list: `make([]string, 0)` is
	// deliberately NOT `append(nil, cidrs...)`, which would collapse a
	// zero-length cidrs back to nil and lose the "explicit []" the request sent.
	stored := make([]string, len(cidrs))
	copy(stored, cidrs)
	row.authorizedCIDRs = stored
	row.rateLimit = req.ApiServerRateLimit // nil clears; never "kept" on omission
	row.mutations++                        // moves on EVERY PUT, identical or not (§3)
	writeJSON(w, http.StatusOK, ns.json(cid, row))
}
