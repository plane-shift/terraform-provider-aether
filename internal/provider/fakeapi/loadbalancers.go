package fakeapi

// Load-balancer route of the fake, reproducing what aether-api DOES (measured
// at aether-api 8e9045a; SDD task-13-measured-contract.md §4) - NOT the stale
// OpenAPI LoadBalancer schema (service_name/namespace/external_ip/dns_name/
// ports), which this fake deliberately does not reproduce:
//
//	GET /organizations/{org}/clusters/{id}/loadbalancers   200 {"items":[{ip,purpose,service_ref?,allocated_at?}]}
//
// Not paginated. service_ref/allocated_at are OMITTED (never null) when
// absent, matching the real handler's `omitempty` (LH:40-45). Real order is
// `allocated_at NULLS LAST, ip` (PI:392-397); SeedLoadBalancers stores items
// VERBATIM in call order and the route re-sorts on every read, so a test can
// seed out of order and still assert on the real order.

import (
	"net/http"
	"regexp"
	"sort"
	"sync"
)

var loadbalancersPathRE = regexp.MustCompile(`^/organizations/([^/]+)/clusters/([^/]+)/loadbalancers$`)

// LoadBalancer is one item SeedLoadBalancers seeds. ServiceRef and
// AllocatedAt nil means absent from the wire.
type LoadBalancer struct {
	IP          string
	Purpose     string // "cp_lb" or "ccm_lb"
	ServiceRef  *string
	AllocatedAt *string // RFC3339 text; nil sorts LAST
}

// lbStore is embedded by value in Server; its zero value is usable.
type lbStore struct {
	mu      sync.Mutex
	items   map[string][]LoadBalancer // clusterID -> items
	rawBody map[string]string         // clusterID -> verbatim 200 body override; see SetLoadBalancersRawBody
}

func (ls *lbStore) init() {
	if ls.items == nil {
		ls.items = map[string][]LoadBalancer{}
	}
	if ls.rawBody == nil {
		ls.rawBody = map[string]string{}
	}
}

// SeedLoadBalancers replaces the load balancers GET …/loadbalancers serves
// for clusterID. Unseeded, a cluster serves an empty list.
func (s *Server) SeedLoadBalancers(clusterID string, items ...LoadBalancer) {
	s.lbs.mu.Lock()
	defer s.lbs.mu.Unlock()
	s.lbs.init()
	s.lbs.items[clusterID] = append([]LoadBalancer(nil), items...)
}

// SetLoadBalancersRawBody forces GET …/loadbalancers to answer 200 with body
// written verbatim, bypassing the normal {"items":[...]} encoding entirely -
// the fake's knob for review finding m5's "body without an items list"
// error-path coverage (a shape the real API's own OpenAPI-drift warning
// makes plausible, even though SeedLoadBalancers alone can never produce
// it). body="" clears the knob and restores normal encoding.
func (s *Server) SetLoadBalancersRawBody(clusterID, body string) {
	s.lbs.mu.Lock()
	defer s.lbs.mu.Unlock()
	s.lbs.init()
	if body == "" {
		delete(s.lbs.rawBody, clusterID)
		return
	}
	s.lbs.rawBody[clusterID] = body
}

// routeLoadBalancers serves GET …/loadbalancers; it reports false when path
// is not that route.
func (s *Server) routeLoadBalancers(w http.ResponseWriter, r *http.Request, path string, _ []byte) bool {
	m := loadbalancersPathRE.FindStringSubmatch(path)
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
	s.lbs.mu.Lock()
	s.lbs.init()
	raw, hasRaw := s.lbs.rawBody[cid]
	all := append([]LoadBalancer(nil), s.lbs.items[cid]...)
	s.lbs.mu.Unlock()
	if hasRaw {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(raw))
		return true
	}
	sort.SliceStable(all, func(i, j int) bool {
		ai, aj := all[i].AllocatedAt, all[j].AllocatedAt
		switch {
		case ai == nil && aj == nil:
			return all[i].IP < all[j].IP
		case ai == nil:
			return false // nil (never allocated) sorts LAST
		case aj == nil:
			return true
		case *ai != *aj:
			return *ai < *aj
		default:
			return all[i].IP < all[j].IP
		}
	})
	items := make([]map[string]any, 0, len(all))
	for _, lb := range all {
		it := map[string]any{"ip": lb.IP, "purpose": lb.Purpose}
		if lb.ServiceRef != nil {
			it["service_ref"] = *lb.ServiceRef
		}
		if lb.AllocatedAt != nil {
			it["allocated_at"] = *lb.AllocatedAt
		}
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
	return true
}
