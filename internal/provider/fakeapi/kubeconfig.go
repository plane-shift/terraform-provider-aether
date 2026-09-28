package fakeapi

// Kubeconfig route of the fake, reproducing what aether-api DOES (measured at
// aether-api 8e9045a; SDD task-13-measured-contract.md §3):
//
//	GET /organizations/{org}/clusters/{id}/kubeconfig   200 RAW YAML | 404 | 409
//
// The response has NO JSON envelope and no key of any kind: Content-Type:
// application/yaml, Content-Disposition: attachment;
// filename="kubeconfig.yaml", body = the admin kubeconfig bytes verbatim.
// Any cluster status other than "running" - INCLUDING "upgrading" and
// "degraded" - is a 409 not_ready "Cluster is not yet ready. Current status:
// <s>" (contract §3). The read PEEKS the cluster's status (statusOf with
// advance=false): a GET of the kubeconfig is a distinct route from a GET of
// the cluster and must not consume the cluster's own StatusSequence walk.

import (
	"fmt"
	"net/http"
	"regexp"
	"sync"
)

var kubeconfigPathRE = regexp.MustCompile(`^/organizations/([^/]+)/clusters/([^/]+)/kubeconfig$`)

// kubeconfigStore is embedded by value in Server; its zero value is usable.
type kubeconfigStore struct {
	mu       sync.Mutex
	content  map[string]string // clusterID -> raw YAML; absent = defaultKubeconfigYAML(id)
	conflict map[string]string // clusterID -> forced 409 message; see SetKubeconfigConflict
}

func (ks *kubeconfigStore) init() {
	if ks.content == nil {
		ks.content = map[string]string{}
	}
	if ks.conflict == nil {
		ks.conflict = map[string]string{}
	}
}

// defaultKubeconfigYAML is what a cluster serves when SetKubeconfig was never
// called for it. It is deterministic from clusterID alone, so a test can
// reconstruct the expected bytes independently instead of trusting the
// fake's own internal copy for a byte-exact comparison.
func defaultKubeconfigYAML(clusterID string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: %s\n  cluster:\n    server: https://%s.example.test:6443\n", clusterID, clusterID)
}

// SetKubeconfig seeds the raw YAML bytes GET …/kubeconfig serves for
// clusterID. Unseeded, a cluster serves defaultKubeconfigYAML(clusterID).
func (s *Server) SetKubeconfig(clusterID, yaml string) {
	s.kubeconfigs.mu.Lock()
	defer s.kubeconfigs.mu.Unlock()
	s.kubeconfigs.init()
	s.kubeconfigs.content[clusterID] = yaml
}

// SetKubeconfigConflict forces GET …/kubeconfig to answer 409 not_ready with
// message, regardless of the cluster's own status - reproducing the real
// API's SECOND 409 cause (review m5/m2): the control plane has not
// published a kubeconfig yet even though the cluster already reads
// "running". Checked before the status-derived 409, so it fires even on a
// running cluster. message="" clears the knob.
func (s *Server) SetKubeconfigConflict(clusterID, message string) {
	s.kubeconfigs.mu.Lock()
	defer s.kubeconfigs.mu.Unlock()
	s.kubeconfigs.init()
	if message == "" {
		delete(s.kubeconfigs.conflict, clusterID)
		return
	}
	s.kubeconfigs.conflict[clusterID] = message
}

// routeKubeconfig serves GET …/kubeconfig; it reports false when path is not
// that route.
func (s *Server) routeKubeconfig(w http.ResponseWriter, r *http.Request, path string, _ []byte) bool {
	m := kubeconfigPathRE.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	org, cid := m[1], m[2]
	if !s.orgKnown(org) {
		apiError(w, http.StatusNotFound, "not_found", "organization not found")
		return true
	}
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, "method_not_allowed", "no route "+r.Method+" "+path)
		return true
	}
	s.clusters.mu.Lock()
	c, ok := s.clusters.clusters[cid]
	if !ok || c.orgID != org {
		s.clusters.mu.Unlock()
		apiError(w, http.StatusNotFound, "not_found", "cluster not found")
		return true
	}
	status := s.clusters.statusOf(c, false) // peek: must not advance the cluster's own StatusSequence walk
	s.clusters.mu.Unlock()
	s.kubeconfigs.mu.Lock()
	s.kubeconfigs.init()
	conflictMsg, forced := s.kubeconfigs.conflict[cid]
	s.kubeconfigs.mu.Unlock()
	if forced {
		apiError(w, http.StatusConflict, "not_ready", conflictMsg)
		return true
	}
	if status != "running" {
		apiError(w, http.StatusConflict, "not_ready", "Cluster is not yet ready. Current status: "+status)
		return true
	}
	s.kubeconfigs.mu.Lock()
	s.kubeconfigs.init()
	content, seeded := s.kubeconfigs.content[cid]
	s.kubeconfigs.mu.Unlock()
	if !seeded {
		content = defaultKubeconfigYAML(cid)
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="kubeconfig.yaml"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
	return true
}
