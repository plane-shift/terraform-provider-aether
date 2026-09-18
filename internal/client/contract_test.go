package client

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// fullSHARe requires exactly 40 LOWERCASE hex characters. A plain length
// check (len(ref) == 40) is satisfied by any 40-byte string, hex or not - it
// would not catch a truncated/garbled ref that happens to be 40 bytes, or an
// uppercase sha copy-pasted from a UI that renders hex in caps (git accepts
// both cases; the oapi-codegen vendoring pipeline that reads this file may
// not).
var fullSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// The provider maps these fields by hand; a rename upstream must fail here, not at a customer.
//
// Every field below was checked against the generated client.gen.go on
// 2026-09-18 (OPENAPI_REF 8e9045a686060112ca27c4f73ce68844322b5c1b) and needed
// NO renames from the task brief — the generator's spelling matched the
// brief's exactly for every type and field listed here.
func TestContract_FieldsTheProviderMapsExist(t *testing.T) {
	want := map[string][]string{
		"Cluster":        {"Id", "Name", "Region", "K8sVersion", "Status", "StatusMessage", "Endpoint", "ObservedKubernetesVersion", "CapiClusterName", "KeycloakRealm", "HarborProject", "CephPool", "LbSubnet", "WorkerSubnet", "PublicEndpoint", "PlanSlug", "CreatedAt", "UpdatedAt"},
		"NodePool":       {"Id", "ClusterId", "Name", "Vcpus", "MemoryGb", "DiskGb", "MinNodes", "MaxNodes", "DesiredNodes", "MinVcpus", "MaxVcpus", "MinMemoryGb", "MaxMemoryGb", "NodeLabels", "NodeTaints", "DrainTimeoutSeconds", "ScaleDownDelaySeconds", "ScaleDownUtilizationPercent", "CurrentNodes", "FirstReadyAt", "Status", "StatusMessage"},
		"ClusterNetwork": {"ClusterId", "AuthorizedCidrs", "ApiServerRateLimit", "UpdatedAt"},
		"Organization":   {"Id", "Name", "Slug", "BillingEmail", "VatId", "Plan"},
		"Member":         {"Id", "Email", "Role", "InvitedBy", "AcceptedAt", "CreatedAt"},
		"Webhook":        {"Id", "Url", "Description", "Events", "Enabled", "Secret"},
		"Registry":       {"Id", "HarborProject", "Endpoint", "Enabled", "QuotaGb", "UsedGb"},
		"RobotAccount":   {"Id", "Name", "Permissions", "HarborId", "LastUsedAt"},
		"CleanupPolicy":  {"Id", "Name", "Action", "Conditions", "RepoScope", "Schedule", "Enabled", "LastRunAt", "LastRunDeleted", "LastRunFreedBytes", "LastRunMessage"},
		"Budget":         {"MonthlyLimitEur", "ThresholdsPct"},
		"K8sVersionInfo": {"Version", "Label", "Patch"},
		"Machine":        {"Name", "Status", "K8sVersion", "InternalIp", "AgeSeconds"},
		"LoadBalancer":   {"ServiceName", "Namespace", "ExternalIp", "DnsName", "Ports"},
	}
	types := map[string]reflect.Type{
		"Cluster": reflect.TypeOf(Cluster{}), "NodePool": reflect.TypeOf(NodePool{}), "ClusterNetwork": reflect.TypeOf(ClusterNetwork{}),
		"Organization": reflect.TypeOf(Organization{}), "Member": reflect.TypeOf(Member{}), "Webhook": reflect.TypeOf(Webhook{}),
		"Registry": reflect.TypeOf(Registry{}), "RobotAccount": reflect.TypeOf(RobotAccount{}), "CleanupPolicy": reflect.TypeOf(CleanupPolicy{}),
		"Budget": reflect.TypeOf(Budget{}), "K8sVersionInfo": reflect.TypeOf(K8sVersionInfo{}), "Machine": reflect.TypeOf(Machine{}), "LoadBalancer": reflect.TypeOf(LoadBalancer{}),
	}
	for name, fields := range want {
		rt, ok := types[name]
		if !ok {
			// rt would otherwise be a nil reflect.Type (the zero value of an
			// interface), and rt.FieldByName below panics on a nil interface
			// receiver rather than reporting a missing entry.
			t.Fatalf("%s has a `want` entry but no matching reflect.TypeOf(...) in `types` in this test", name)
		}
		for _, f := range fields {
			if _, ok := rt.FieldByName(f); !ok {
				t.Errorf("%s.%s missing from the generated client — upstream renamed it; regenerate and update the provider", name, f)
			}
		}
	}
}

func TestContract_OpenAPIRefIsAFullSHA(t *testing.T) {
	b, err := os.ReadFile("../../OPENAPI_REF")
	if err != nil {
		t.Fatal(err)
	}
	ref := strings.TrimSpace(string(b))
	if !fullSHARe.MatchString(ref) {
		t.Fatalf("OPENAPI_REF = %q, want exactly 40 lowercase hex characters", ref)
	}
}

func TestContract_OperationsTheProviderCallsExist(t *testing.T) {
	c := &ClientWithResponses{}
	ct := reflect.TypeOf(c)
	for _, m := range []string{"ListOrganizationsWithResponse", "GetOrganizationWithResponse", "UpdateOrganizationWithResponse",
		"ListClustersWithResponse", "CreateClusterWithResponse", "GetClusterWithResponse", "UpdateClusterWithResponse", "DeleteClusterWithResponse", "UpgradeClusterWithResponse",
		"GetClusterNetworkWithResponse", "UpsertClusterNetworkWithResponse", "GetKubeconfigWithResponse", "ListLoadBalancersWithResponse", "ListClusterVolumesWithResponse",
		"ListNodePoolsWithResponse", "CreateNodePoolWithResponse", "GetNodePoolWithResponse", "UpdateNodePoolWithResponse", "DeleteNodePoolWithResponse", "ListNodePoolMachinesWithResponse",
		"ListMembersWithResponse", "CreateMemberWithResponse", "GetMemberWithResponse", "UpdateMemberWithResponse", "DeleteMemberWithResponse",
		"ListWebhooksWithResponse", "CreateWebhookWithResponse", "GetWebhookWithResponse", "UpdateWebhookWithResponse", "DeleteWebhookWithResponse", "RotateWebhookSecretWithResponse",
		"GetRegistryWithResponse", "EnableRegistryWithResponse", "DisableRegistryWithResponse", "ListRepositoriesWithResponse", "ListRepositoryTagsWithResponse",
		"ListRobotAccountsWithResponse", "CreateRobotAccountWithResponse", "DeleteRobotAccountWithResponse",
		"ListCleanupPoliciesWithResponse", "CreateCleanupPolicyWithResponse", "UpdateCleanupPolicyWithResponse", "DeleteCleanupPolicyWithResponse",
		"GetBudgetWithResponse", "SetBudgetWithResponse", "DeleteBudgetWithResponse", "GetUsageWithResponse", "GetCostBreakdownWithResponse", "ListInvoicesWithResponse", "ListAuditLogWithResponse",
		"ListRegionsWithResponse", "ListK8sVersionsWithResponse"} {
		if _, ok := ct.MethodByName(m); !ok {
			t.Errorf("client method %s missing — operationId changed upstream", m)
		}
	}
}
