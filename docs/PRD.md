### 7.2 Terraform Provider Compatibility

The API is consumed by `terraform-provider-aether` (registry address `plane-shift/aether`, to be published from its first release, v0.1.0, on the Terraform Registry and the OpenTofu Registry; Terraform 1.5+ and OpenTofu 1.6+ are both supported). The provider's generated docs (`docs/index.md`, `docs/resources/*.md`, `docs/data-sources/*.md` in that repository) and its validated examples (`examples/`) are the authoritative reference for attribute names and behaviour. This section records the API properties the provider depends on, **as measured against the API** (the per-task measured contracts in the provider's SDD ledger); where the API falls short of a principle, the gap and the provider's answer are stated rather than the aspiration.

#### 7.2.1 Design Principles for Terraform Compatibility

| Principle | Requirement | Measured reality / provider answer |
|---|---|---|
| **Full CRUD on every mutable resource** | Every resource supports Create (POST), Read (GET), Update (PUT/PATCH), Delete (DELETE) | Holds for clusters, pools, webhooks, cleanup policies. Gaps the provider works around: organizations are created at registration, so `aether_organization` is **import-only** (create is refused; destroy only forgets it); member `PUT` is **403 for every API token**, so a `role` change is a replacement (DELETE + POST); robot accounts have **no GET-by-id** (the provider pages the list); the billing budget and cluster network are singletons with PUT-upsert. |
| **Creates are NOT idempotent** | A repeated POST must not silently create a duplicate | The API returns a conflict rather than the existing resource: cluster names are unique per organization (`UNIQUE(org_id, name)`), live pool names are unique per cluster (a `deleting` pool keeps its name until the sweep removes it) → **409**; a second registry enable is **409**; the webhook cap (10) is **409 `webhook_limit`**. The budget `PUT` is an upsert, so the provider reads first and refuses to overwrite a budget set in the portal (with an import hint). |
| **PUT semantics differ per resource** | Terraform sends the full desired state on every apply | Cluster network and cleanup policies are **full replace** (an omitted field is reset); webhooks are a **partial merge**; organization fields are a COALESCE merge (`""` clears, NULL is unreachable). A pool `PUT` with only `desired_nodes` can be refused or silently set min = max = desired, so the provider always sends the full sizing. |
| **Stable resource IDs** | All resources identified by immutable UUID | Holds. Every `format: uuid` is canonical UUID text in state. An organization's `slug` is immutable from the provider (a rename would strand registry and storage billing). |
| **Asynchronous lifecycle with polled status** | Mutating operations return immediately; GET reports progress | Cluster: success = `running`; `degraded` = success with a warning; `failed` = error; `provisioning` / `upgrading` / `deleting` are transient. Node pool: success = `running`; `provisioning`, `scaling`, `scaling-down` transient; `deleting` is an error while waiting for `running` (a pool being deleted never reaches it); `degraded` clears by itself (`ready` and `failed` are never written for pools). See §7.2.5. |
| **Computed vs configured fields** | The client must know which fields it manages and which it reads | **OpenAPI `readOnly` is NOT what marks computed fields — the provider decides per attribute** (Optional / Computed / RequiresReplace / UseStateForUnknown). Several wire fields are absent from the spec entirely: robot `harbor_username` and secret `token`, list `next_cursor`, tag `vulnerabilities`, invoice `invoice_number`; the provider decodes those responses raw. |
| **Import support** | GET by ID returns enough to reconstruct state | Holds except for secrets: robot secrets and the registry's enable credentials are returned **only on create**, and webhook signing secrets **on create and on rotation** (the only secret the provider rotates), so they are null after an import. The registry credentials are also nulled when the registry id changes. |
| **Deterministic defaults** | Server-side defaults are documented and deterministic. If a field is omitted, the default value appears in the GET response. **Exception — the unset-tracking pool fields:** an unset `drain_timeout_seconds`, `scale_down_delay_seconds` or `scale_down_utilization_percent` stays **absent** in the GET response rather than materialising as 600/600/50, because absence is what makes the pool keep inheriting the platform default if that default later moves. The Terraform mapping must **preserve absence as null** and never write those defaults back as explicit settings — doing so would silently pin the pool to today's value. | Holds for pools (with the exception). It does **not** hold for cleanup policies: the API validates nothing and an omitted `action` / `schedule` / `repo_scope` is a **500** (aether-api#311), so the provider always sends its own defaults (`delete` / `nightly` / `[]` / enabled). Once set, `drain_timeout_seconds` and the `scale_down_*` fields cannot be cleared through the API. |
| **No nested resource creation** | Creating a cluster does NOT auto-create a node pool | Holds. Node pools, the cluster network, registry robot accounts and cleanup policies are separate resources. |
| **Structured errors** | 4xx errors are machine-readable | The envelope is `{"error":{"code","message","details":[{field,code,message}]}}`; the request id is only in the `X-Request-ID` response header. Several statuses are undocumented per route (500s on invalid input, 400s on unknown fields). |
| **Pagination is NOT uniform** | Data sources must enumerate every item | Clusters, node pools, members and webhooks use `?cursor=<uuid>&limit=N` (default 50, max 100) and return `next_cursor` **only on a full page**. Robot accounts: a fixed page of 50, no `next_cursor`. Cleanup policies: unpaginated. Registry repositories and tags: Harbor page numbers, read live. Audit log and invoices: cursor unreliable past page 1 (aether-api#318) — the audit-log data source reads **one page** (`limit` input). A repeated cursor is an error in every provider pager. |
| **No name filtering** | Data sources need lookups such as "the cluster named prod" | **No list endpoint accepts `?name=`**; the only server-side filter is `?status=` on clusters. The provider's singular data sources filter client-side after paging. |
| **Kubeconfig converges** | The downloaded kubeconfig must target the current endpoint | The admin kubeconfig follows the public endpoint when it changes; a copy already read into state is detached until the next refresh. It is served only while the cluster is `running` (any other status → 409). |

**Billing properties a Terraform user triggers** (stated once here; §22 is authoritative): `public_endpoint` defaults to **true** in the provider (the API default is `false`) and bills one public IP at **€4/month**; fixed node pools and the control plane bill a **flat monthly** fee; autoscaling pools (budget-based: `max_vcpus > min_vcpus` or `max_memory_gb > min_memory_gb`) bill **hourly per node**.

#### 7.2.2 Terraform Resource Mapping

The organization is a **provider-level** setting: `organization_id` is optional on the provider (resolved from the API token, validated when given) and appears on **no** resource. There is no `region` on the provider; the region string is `eu-fi` everywhere. **API tokens are created in the portal only — there is no token resource.**

**Resources (10):**

| API Resource | Terraform Resource | Key behaviour |
|---|---|---|
| Cluster | `aether_cluster` | name, region, k8s_version (forward-only in-place upgrade), `public_endpoint` (default true, €4/month); create waits for `running` |
| Node Pool | `aether_node_pool` | cluster_id, name, vcpus, memory_gb, disk_gb (name and sizes RequiresReplace), desired_nodes, autoscaling budget (min/max_vcpus, min/max_memory_gb), node_labels, node_taints, drain_timeout_seconds (30–3600), scale_down_delay_seconds (60–3600), scale_down_utilization_percent (10–90) — the last three unset-tracking |
| Cluster Network | `aether_cluster_network` | cluster_id, authorized_cidrs (canonical IPv4 prefixes), api_server_rate_limit; create overwrites the current policy, **destroy opens the endpoint to any source** (empty list = allow all) |
| Organization | `aether_organization` | **import-only** (create is refused); name, billing_email, vat_id; `slug` Optional but immutable (any other value is refused at plan time) |
| Organization Member | `aether_organization_member` | email, role (**change = replace**: API tokens cannot `PUT` a member); provider-side last-owner guard |
| Webhook | `aether_webhook` | url, events, description, enabled, `rotate_secret` trigger; `secret` only on create/rotate; at most 10 per organization |
| Registry | `aether_registry` | organization singleton; `purge_on_destroy` (default false — images are kept); destroy disables the registry and deletes **every** robot account; enable credentials write-once |
| Registry Robot Account | `aether_registry_robot_account` | name, permissions ⊂ {push, pull, delete} (RequiresReplace); `secret` only on create |
| Registry Cleanup Policy | `aether_registry_cleanup_policy` | name, action (`delete`/`protect`), schedule, repo_scope, conditions, enabled; an enabled delete policy runs within about a minute |
| Billing Budget | `aether_billing_budget` | monthly_limit_eur, thresholds_pct (a `usage.alert` webhook per threshold); advisory only (never stops workloads); organization singleton, create refuses to overwrite a portal-set budget |

**Data sources (23, all read-only):**

| Scope | Terraform Data Sources |
|---|---|
| Organization & platform | `aether_organization` (the token's organization — takes no `slug`), `aether_regions`, `aether_k8s_versions` (`latest` is null while the version window is unavailable) |
| Clusters | `aether_cluster`, `aether_clusters`, `aether_cluster_network`, `aether_kubeconfig` (**the ADMIN kubeconfig — privileged**; only while the cluster is `running`), `aether_loadbalancers` (`{ip, purpose, service_ref, allocated_at}`), `aether_cluster_volumes` |
| Node pools | `aether_node_pool`, `aether_node_pools`, `aether_node_pool_machines` |
| Access | `aether_organization_members`, `aether_webhooks` |
| Registry | `aether_registry`, `aether_registry_repositories`, `aether_registry_repository_tags`, `aether_registry_robot_accounts`, `aether_registry_cleanup_policies` |
| Audit & billing | `aether_audit_log` (one page), `aether_usage` (current UTC month), `aether_cost_breakdown`, `aether_invoices` |

Out of scope for the provider: payment methods, support tickets, admin routes.

#### 7.2.3 Example Terraform Usage

HCL is **not** hand-written here. The examples below are copied verbatim from the provider's `examples/` directory, which CI validates on both Terraform and OpenTofu; the provider block itself is `examples/provider/provider.tf` (source `plane-shift/aether`, version `~> 0.1`; `api_token` or `AETHER_API_TOKEN`; `endpoint` defaults to `https://api.aetherplatform.cloud`, `https://dev.api.aetherplatform.cloud` for dev; optional `organization_id`). When the provider changes, re-copy from there.

Cluster, pools and kubeconfig (`examples/resources/aether_cluster/resource.tf`):

```hcl
data "aether_k8s_versions" "offered" {}

resource "aether_cluster" "prod" {
  name   = "prod"
  region = "eu-fi"
  # latest is null while the platform's version window is unavailable; pin a
  # minor instead (e.g. "1.37") if that's not acceptable.
  k8s_version = data.aether_k8s_versions.offered.latest
  # Defaults to true: the public API endpoint is billed as one public IP (€4/month).
  public_endpoint = true
}

# Fixed pool: a flat monthly fee for desired_nodes nodes.
resource "aether_node_pool" "workers" {
  cluster_id    = aether_cluster.prod.id
  name          = "workers"
  vcpus         = 4
  memory_gb     = 16
  disk_gb       = 80
  desired_nodes = 3
}

# Autoscaling pool: a vCPU/memory budget with max > min makes it autoscaling (billed hourly per node).
resource "aether_node_pool" "burst" {
  cluster_id    = aether_cluster.prod.id
  name          = "burst"
  vcpus         = 4
  memory_gb     = 16
  disk_gb       = 80
  min_vcpus     = 4
  max_vcpus     = 32
  min_memory_gb = 16
  max_memory_gb = 128
}

data "aether_kubeconfig" "prod" {
  cluster_id = aether_cluster.prod.id
}

output "kubeconfig" {
  value     = data.aether_kubeconfig.prod.content
  sensitive = true
}
```

Organization import (`examples/resources/aether_organization/resource.tf` + `import.sh`):

```hcl
# Import-only: this resource cannot create an organization (see the import command below).
# The slug cannot be changed from Terraform; "" clears an optional field.
resource "aether_organization" "this" {
  name          = "Example Ltd"
  billing_email = "billing@example.com"
  vat_id        = "FI12345678"
}
```

```sh
# Organizations are created at registration; adopt yours by id (data.aether_organization exposes it).
terraform import aether_organization.this 00000000-0000-0000-0000-000000000000
# OpenTofu: tofu import aether_organization.this 00000000-0000-0000-0000-000000000000
```

Webhook (`examples/resources/aether_webhook/resource.tf`):

```hcl
variable "webhook_secret_rotation" {
  type    = string
  default = "2026-09"
}

# The signing secret is returned only on create and on rotation; change
# rotate_secret to rotate it. At most 10 webhooks per organization.
resource "aether_webhook" "alerts" {
  url           = "https://hooks.example.com/aether"
  description   = "Cluster lifecycle and budget alerts"
  events        = ["cluster.ready", "cluster.degraded", "usage.alert"]
  rotate_secret = var.webhook_secret_rotation
}

output "webhook_secret" {
  value     = aether_webhook.alerts.secret
  sensitive = true
}
```

Registry, robot account and cleanup policy (`examples/resources/aether_registry/resource.tf`):

```hcl
# Destroying the registry disables it and deletes EVERY robot account of the organization;
# images are kept unless purge_on_destroy = true (irreversible).
resource "aether_registry" "this" {
  purge_on_destroy = false
}

# depends_on makes Terraform destroy the robot account before the registry.
resource "aether_registry_robot_account" "ci" {
  name        = "ci"
  permissions = ["push", "pull"]

  depends_on = [aether_registry.this]
}

# An enabled nightly/weekly "delete" policy runs within about a minute of the apply:
# start with action = "protect" or enabled = false.
resource "aether_registry_cleanup_policy" "old_untagged" {
  name     = "old-untagged"
  action   = "protect"
  schedule = "nightly"
  enabled  = false

  conditions = [
    { type = "untagged", value = "true" },
    { type = "age", op = "gt", value = "30d" },
  ]

  depends_on = [aether_registry.this]
}

output "robot_secret" {
  value     = aether_registry_robot_account.ci.secret
  sensitive = true
}
```

#### 7.2.4 API Response Contract for Terraform

Responses are **flat JSON objects** — the resource's fields at the top level, with no envelope and no split between user-set and system-computed fields. Example (cluster, abridged):

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "production",
  "region": "eu-fi",
  "k8s_version": "1.37",
  "status": "running",
  "created_at": "2026-04-04T12:00:00Z",
  "updated_at": "2026-04-04T12:05:00Z"
}
```

Which fields are configured, computed or both is decided **by the provider, per attribute**, from the measured behaviour of each route — not from OpenAPI `readOnly`, which does not carry that decision (and the spec omits several wire fields entirely). List endpoints return `{"items": [...]}` plus `next_cursor` where cursor-paginated (see the pagination row of §7.2.1); a few routes return a bare object instead (e.g. cluster volumes, with `inventory_available`). The admin kubeconfig is served as raw `application/yaml`, not JSON; the generated Go client cannot decode it, so the provider reads the body directly (aether-api#316).

#### 7.2.5 Async Operation Polling

Long-running operations (cluster create, upgrade, delete; pool create and resize) follow this pattern:

```
POST /api/v1/organizations/{org}/clusters
→ 202 Accepted   { "id": "...", "status": "provisioning", ... }

GET  /api/v1/organizations/{org}/clusters/{id}
→ 200 OK         { "id": "...", "status": "provisioning", ... }   // poll

// Cluster:   running = success; degraded = success + warning; failed = error
//            provisioning / upgrading / deleting = keep polling
// Node pool: running = success; provisioning / scaling / scaling-down = keep polling;
//            deleting = error while waiting for running (a deleted pool never reaches it);
//            degraded (with status_message) clears by itself once the nodes recover
// Delete:    poll until GET returns 404
```

The provider polls GET with backoff inside the resource's `timeouts` until a terminal state. The API's guarantees, as measured:
- **Status is not monotonic.** A `running` cluster moves to `upgrading` and back; a pool moves between `running`, `scaling`, `scaling-down` and `degraded`. Only `deleting` is one-way.
- **`status_message`** accompanies `degraded` and `failed` where the platform has an explanation.
- **Delete ends in 404** — only a 404 means gone; any other error is surfaced, never read as deletion.
- **Pool updates have no convergence signal** except a node-count change on a fixed pool or a move from autoscaling to fixed (both wait until every desired node is ready); every other update returns once the API accepts it. The cluster network has no convergence signal at all, so the provider never waits on it.

