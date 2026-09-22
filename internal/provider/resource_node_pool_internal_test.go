package provider

// M-3a (review round 1/2): terraform-plugin-testing v1.16.0's TestStep has no
// facility to assert on a successful step's non-error diagnostics, so the
// "Node pool is degraded" warning Update raises on a non-waiting path
// (resource_node_pool.go, warnIfDegraded call sites) cannot be pinned through
// the black-box harness tests in resource_node_pool_test.go. warnIfDegraded
// itself is a pure function, so it is pinned directly here instead, in the
// internal package (see provider_test.go for the same pattern).

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
)

// Named Test_warnIfDegraded (not TestWarnIfDegraded) so `-run warnIfDegraded`
// matches: Go only treats a name as a test if the character after "Test" is
// not a lowercase letter, so the literal function name has to carry an
// underscore before the lowercase substring.
func Test_warnIfDegraded_DegradedWarnsWithTheMessageRunningDoesNot(t *testing.T) {
	msg := "Available=False: MinimumReplicasUnavailable"
	degraded := &client.NodePool{Status: client.NodePoolStatus(poolStatusDegraded), StatusMessage: &msg}
	var diags diag.Diagnostics
	warnIfDegraded(degraded, &diags)
	want := diag.Diagnostics{diag.NewWarningDiagnostic("Node pool is degraded", msg)}
	if !diags.Equal(want) {
		t.Fatalf("degraded diagnostics = %v, want exactly %v", diags, want)
	}

	running := &client.NodePool{Status: client.NodePoolStatus(poolStatusRunning)}
	diags = nil
	warnIfDegraded(running, &diags)
	if len(diags) != 0 {
		t.Fatalf("running diagnostics = %v, want none", diags)
	}
}
