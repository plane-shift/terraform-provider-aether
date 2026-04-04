### 7.2 Terraform Provider Compatibility

The API is designed from day one to support a future `terraform-provider-aether`. Every mutable resource follows strict CRUD conventions so that the Terraform provider is a thin mapping layer, not a workaround layer.

#### 7.2.1 Design Principles for Terraform Compatibility

| Principle | Requirement | Rationale |
|---|---|---|
| **Full CRUD on every mutable resource** | Every resource supports Create (POST), Read (GET), Update (PUT/PATCH), Delete (DELETE) | Terraform resources map 1:1 to CRUD endpoints. Missing any verb forces hacks in the provider. |
| **Idempotent creates** | POST with a client-supplied `name` returns existing resource if it already exists (or 409 with existing ID) | `terraform apply` may re-run creates. Idempotency prevents duplicates. |
| **PUT for full update, PATCH for partial** | PUT replaces the full resource spec. PATCH applies a partial diff. | Terraform sends the full desired state on every apply — PUT is the natural fit. PATCH supports portal/CLI partial updates. |
| **Stable resource IDs** | All resources identified by immutable UUID. `name` is mutable, `id` is not. | Terraform tracks resources by ID in state. If the ID changes, Terraform treats it as destroy+recreate. |
| **Synchronous status + async operations** | Mutating operations return immediately with `status: "provisioning"`. GET returns current status. | Terraform provider polls GET until status reaches a terminal state (`running`, `ready`, `failed`). |
| **Computed vs configured fields** | Response clearly separates: fields the user sets (configured) vs fields the system computes (endpoint, IP, status, created_at) | Terraform must distinguish between what it manages and what it reads. Computed fields are marked `readOnly` in OpenAPI. |
| **Import support** | GET by ID returns the full resource state, identical to what POST/PUT returns | `terraform import` reads existing resources into state. The GET response must be complete enough to reconstruct state. |
| **Deterministic defaults** | Server-side defaults are documented and deterministic. If a field is omitted, the default value appears in the GET response. | Terraform compares desired state vs actual state. If defaults are invisible, Terraform sees phantom drift on every plan. |
| **No nested resource creation** | Creating a cluster does NOT auto-create a node pool. Node pools are separate resources. | Terraform manages each resource independently. Implicit child creation causes state mismatches. |
| **Meaningful error responses** | 400/409/422 errors include `field`, `code`, and `message` | Terraform provider surfaces errors to the user. Structured errors enable better UX. |
| **Pagination** | All list endpoints use cursor-based pagination (`?cursor=xxx&limit=50`) | Terraform data sources must enumerate all resources. Cursor pagination is stateless and consistent under concurrent writes. |
| **Filtering** | List endpoints support query params for filtering (`?status=running&name=prod`) | Terraform data sources need filtered lookups (e.g., "find the cluster named prod"). |

#### 7.2.2 Terraform Resource Mapping

Every API resource maps to a Terraform resource or data source:

| API Resource | Terraform Resource | Terraform Data Source | Key Attributes |
|---|---|---|---|
| Organization | `aether_organization` | `aether_organization` (by slug) | name, slug, billing_email, vat_id |
| Cluster | `aether_cluster` | `aether_cluster` / `aether_clusters` | name, region, k8s_version, endpoint (computed) |
| Node Pool | `aether_node_pool` | `aether_node_pool` / `aether_node_pools` | cluster_id, name, machine_tier, min_nodes, max_nodes, desired_nodes |
| Cluster Network | `aether_cluster_network` | `aether_cluster_network` | cluster_id, authorized_cidrs |
| Member | `aether_organization_member` | `aether_organization_members` | org_id, email, role |
| API Token | `aether_api_token` | — | org_id, name, scopes, token (sensitive, computed, write-once) |
| Webhook | `aether_webhook` | `aether_webhooks` | org_id, url, events, secret (sensitive) |
| Registry Robot Account | `aether_harbor_robot_account` | `aether_harbor_robot_accounts` | cluster_id, name, permissions |

**Read-only data sources** (no corresponding resource — not user-creatable):

| API Resource | Terraform Data Source | Purpose |
|---|---|---|
| Regions | `aether_regions` | List available regions for cluster creation |
| Kubeconfig | `aether_kubeconfig` | Download OIDC kubeconfig for a cluster |
| LoadBalancers | `aether_loadbalancers` | List LBs for a cluster (reflects K8s state) |
| Usage | `aether_usage` | Current billing period usage |
| Invoices | `aether_invoices` | Invoice history |

#### 7.2.3 Example Terraform Usage (Target State)

```hcl
# terraform-provider-aether — target API shape

terraform {
  required_providers {
    aether = {
      source  = "aether-cloud/aether"
      version = "~> 1.0"
    }
  }
}

provider "aether" {
  api_token = var.aether_api_token
  region    = "eu-de"               # regional API endpoint
}

# Organization is typically imported, not created via Terraform
data "aether_organization" "main" {
  slug = "my-company"
}

# Cluster — full CRUD
resource "aether_cluster" "production" {
  organization_id = data.aether_organization.main.id
  name            = "production"
  region          = "eu-de"
  k8s_version     = "1.31"

  # No node pools here — they are separate resources
}

# Node pool — separate resource, depends on cluster
resource "aether_node_pool" "workers" {
  organization_id = data.aether_organization.main.id
  cluster_id      = aether_cluster.production.id
  name            = "default-workers"
  machine_tier    = "medium"        # 4 vCPU, 16 GB
  min_nodes       = 2
  max_nodes       = 8
  desired_nodes   = 3
}

resource "aether_node_pool" "highmen" {
  organization_id = data.aether_organization.main.id
  cluster_id      = aether_cluster.production.id
  name            = "highmem"
  machine_tier    = "large"         # 8 vCPU, 32 GB
  min_nodes       = 1
  max_nodes       = 4
  desired_nodes   = 1
}

# Network policy — restrict kubectl access to VPN
resource "aether_cluster_network" "production" {
  organization_id = data.aether_organization.main.id
  cluster_id      = aether_cluster.production.id
  authorized_cidrs = [
    "10.0.0.0/8",       # Corporate VPN
    "203.0.113.0/32",   # Office egress
  ]
}

# Team member
resource "aether_organization_member" "dev_lead" {
  organization_id = data.aether_organization.main.id
  email           = "alex@company.com"
  role            = "admin"
}

# Webhook for CI/CD notifications
resource "aether_webhook" "slack_notifications" {
  organization_id = data.aether_organization.main.id
  url             = "https://hooks.slack.com/services/T00/B00/xxx"
  events          = ["cluster.ready", "cluster.degraded", "security.cve_critical"]
}

# Read-only — kubeconfig for downstream providers
data "aether_kubeconfig" "production" {
  organization_id = data.aether_organization.main.id
  cluster_id      = aether_cluster.production.id
}

# Outputs
output "cluster_endpoint" {
  value = aether_cluster.production.endpoint
}

output "kubeconfig" {
  value     = data.aether_kubeconfig.production.content
  sensitive = true
}
```

#### 7.2.4 API Response Contract for Terraform

Every mutable resource response follows this structure:

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "production",
  "status": "running",
  "created_at": "2026-04-04T12:00:00Z",
  "updated_at": "2026-04-04T12:05:00Z",

  "_configured": {
    "name": "production",
    "region": "eu-de",
    "k8s_version": "1.31"
  },

  "_computed": {
    "endpoint": "https://production.k8s.eu-de.aether.cloud:6443",
    "status": "running",
    "keycloak_realm": "production",
    "harbor_project": "production",
    "lb_subnet": "10.100.3.0/24"
  }
}
```

The `_configured` / `_computed` split is metadata to aid Terraform provider development. The top-level object remains a flat JSON structure for general API consumers. OpenAPI `readOnly: true` marks computed fields — the provider uses this annotation to auto-generate the schema.

#### 7.2.5 Async Operation Polling

Long-running operations (cluster create, upgrade, delete) follow a consistent pattern:

```
POST /api/v1/orgs/{id}/clusters
→ 202 Accepted
{
  "id": "...",
  "status": "provisioning",
  ...
}

GET /api/v1/orgs/{id}/clusters/{cid}
→ 200 OK
{
  "id": "...",
  "status": "provisioning",   // Terraform polls until terminal
  ...
}

// Terminal states: "running", "failed", "deleting", "deleted"
// Terraform provider uses sdk.StateChangeConf to poll
```

The Terraform provider polls GET with exponential backoff until status reaches a terminal state. The API guarantees:
- **Status transitions are monotonic** — a resource never goes from "running" back to "provisioning"
- **Failed states include a `status_message`** explaining what went wrong
- **Delete returns 404 after completion** — Terraform interprets 404 on read as "resource gone", which is correct after deletion

