package provider_test

// Shared helpers for the Task 13 data-source tests (datasource_*_test.go).
// Every data source in this task is currently UNREGISTERED
// (provider.go DataSources() still returns []) - every test in every
// datasource_*_test.go file in this package therefore currently fails at
// PLAN time with the exact same Terraform-core diagnostic:
//
//	Error: Invalid data source
//	The provider hashicorp/aether does not support data source "aether_<name>".
//
// (measured empirically against this worktree's HEAD, not assumed). That is
// the CURRENT failure reason for every test below; each test's own doc
// comment says what it proves once the data source exists, i.e. how it can
// ALSO fail against a wrong implementation.

import (
	"fmt"
	"regexp"
)

// strp is already defined in resource_organization_test.go (package-wide, a
// pointer to s for the fake's *string knob arguments - SetOrgBilling,
// LoadBalancer.ServiceRef/AllocatedAt - and reused here).

// exactlyOneOfErrorRE tolerates either shape the framework's ExactlyOneOf
// validator can produce depending on whether the coder attaches it
// per-attribute (stringvalidator.ExactlyOneOf: "one (and only one) of ... is
// required") or at the data-source level (datasourcevalidator.ExactlyOneOf:
// "Exactly one of these attributes must be configured") - the brief pins the
// CONTRACT (exactly one of id/name), not which mechanism implements it.
var exactlyOneOfErrorRE = regexp.MustCompile(`(?is)exactly one of|one\s*\(and only one\)\s*of`)

// dsOrgProvider is the provider block every data-source test needs; setupFake
// already wires AETHER_ENDPOINT/AETHER_API_TOKEN and leaves
// AETHER_ORGANIZATION_ID empty so Configure auto-resolves the fake's single
// registered org (testOrg).
const dsProviderBlock = `provider "aether" {}` + "\n"

// clusterDataSourceHCL renders `data "aether_cluster" "c"` with whichever of
// id/name (or neither/both) the test wants to exercise.
func clusterDataSourceHCL(id, name string) string {
	body := ""
	if id != "" {
		body += fmt.Sprintf("  id   = %q\n", id)
	}
	if name != "" {
		body += fmt.Sprintf("  name = %q\n", name)
	}
	return dsProviderBlock + fmt.Sprintf("data \"aether_cluster\" \"c\" {\n%s}\n", body)
}

// nodePoolDataSourceHCL renders `data "aether_node_pool" "p"`.
func nodePoolDataSourceHCL(clusterID, id, name string) string {
	body := fmt.Sprintf("  cluster_id = %q\n", clusterID)
	if id != "" {
		body += fmt.Sprintf("  id   = %q\n", id)
	}
	if name != "" {
		body += fmt.Sprintf("  name = %q\n", name)
	}
	return dsProviderBlock + fmt.Sprintf("data \"aether_node_pool\" \"p\" {\n%s}\n", body)
}
