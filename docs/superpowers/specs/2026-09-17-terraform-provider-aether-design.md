# terraform-provider-aether — design

Date: 2026-09-17. Status: approved in brainstorming with the owner; awaiting written review.
Companion: aether-cli (Rust) is a separate sub-project that mirrors this provider's surface
and is designed after this provider ships.

## 1. Goal and scope

A Terraform **and OpenTofu** provider, `plane-shift/aether`, that manages everything a
tenant can manage through the Aether API for cluster, access and registry, plus the
billing budget, and exposes every read-only view as a data source.

**Owner rulings (2026-09-17):**

- Provider first, CLI second; the CLI mirrors the provider's proven surface.
- Coverage: cluster, node pools, cluster network, organization (imported), members,
  webhooks, registry, robot accounts, cleanup policies, billing budget. **Out:** payment
  methods (Stripe, portal-only), tickets (support workflow), admin routes (platform
  operators), and **API tokens** — tokens are created in the portal and are a
  prerequisite for using the provider and the CLI; the provider only consumes one.
- Distribution: the repo `plane-shift/terraform-provider-aether` goes **public** (the
  registry requires the `terraform-provider-<name>` repo name) and publishes as
  `plane-shift/aether` on the Terraform Registry and the OpenTofu Registry.
- Organization scoping is **provider-level**: an API token belongs to one organization
  (the one created at registration) and the provider resolves it automatically; resources
  carry no `organization_id`. A `data "aether_organization"` exposes it.
- Acceptance tests run against **dev** (`https://dev.api.aetherplatform.cloud`) with a
  dedicated organization and token the owner creates in the portal; never on every PR.
- Implementation approach A: `terraform-plugin-framework` + an OpenAPI-generated client +
  hand-written resource logic (see §3).

**PRD drift to correct (aether-docs `docs/PRD.md` §7.2, in the docs wave):** the example
uses `eu-de`, `machine_tier`, `k8s_version = "1.31"`, per-resource `organization_id`, an
`aether_api_token` resource, a `_configured/_computed` response split and "computed fields
are `readOnly` in OpenAPI". None of these match the live API: the region is `eu-fi`, pools
are sized by exact `vcpus`/`memory_gb`/`disk_gb`, autoscaling is a vCPU/memory budget, the
responses are flat, and only `Organization.plan` carries `readOnly`. The provider decides
configured-vs-computed per resource by hand (§4).

## 2. Repository layout

```
main.go                         # serves the provider (plugin protocol v6)
api/openapi.yaml                # vendored from aether-api at OPENAPI_REF (never hand-edited)
OPENAPI_REF                     # full aether-api commit SHA the spec came from
scripts/fetch-openapi.sh        # re-vendor at a given ref (same shape as aether-docs)
internal/client/                # oapi-codegen output — `go generate ./...`; never hand-edited
internal/provider/
  provider.go                   # provider schema + Configure (token, endpoint, org resolution)
  resource_<name>.go            # one file per resource: schema, Create/Read/Update/Delete, ImportState
  datasource_<name>.go          # one file per data source
  apierr/                       # API error envelope → diagnostics
  transport/                    # RoundTripper: bearer auth + bounded retries
  versions/                     # k8s_version custom type and semantic equality
  wait/                         # poll-to-terminal-state helper; framework `timeouts` integration
  fakeapi/                      # in-process API fake for unit tests; NOT a _test.go file, so every package can use it
internal/acctest/               # acceptance harness (provider factories, env, cleanup)
docs/                           # tfplugindocs output (registry format); docs/PRD.md stays
examples/, templates/           # tfplugindocs inputs
.goreleaser.yml                 # registry platform matrix, GPG signing, registry manifest
terraform-registry-manifest.json
.github/workflows/              # build-test.yml (PR), release.yml (v* tags), acceptance.yml (manual/nightly)
```

Rules: client code changes only by regeneration; each resource file has one
responsibility and depends only on the client, `wait`, `versions` and `apierr`.

## 3. Components

### 3.1 Provider configuration

```hcl
provider "aether" {
  api_token       = var.aether_api_token   # or AETHER_API_TOKEN
  endpoint        = "https://api.aetherplatform.cloud"   # default; dev: https://dev.api.aetherplatform.cloud
  organization_id = "…"                    # optional
}
```

`api_token` is `Sensitive` in the schema (masked in CLI output) and is never written to
diagnostics or logs — messages name the attribute, never its value. `Configure` builds the
client with `Authorization: Bearer <token>`, then resolves the organization: if
`organization_id` is set it is **validated** with `GET /api/v1/organizations/{id}` (a 403/404
means the token does not belong to that organization → diagnostic); otherwise
`GET /api/v1/organizations` must return exactly one organization, which is used; zero or
several → a diagnostic naming the count and asking for `organization_id`. Two organizations in one root module use two
provider aliases with two tokens (tokens are per-organization by construction).

### 3.2 Generated client

`oapi-codegen` (client + types, `net/http`) from the vendored spec. `OPENAPI_REF` pins the
aether-api commit. A `make openapi` target re-vendors and regenerates; CI fails if
`go generate` produces a diff (the checked-in client must match the checked-in spec).

### 3.3 Wait helper

`wait.ForStatus(ctx, probe, Options{Timeout, Success, Warning, Failed, Now, Sleep})` polls
until the status is terminal. Three rules are load-bearing:

- **The status is matched on every probe, including the first** — check before sleep. A
  sleep-then-check loop makes the fastest operation take a whole backoff interval, and hides
  a resource that was already terminal when the wait began.
- **A `Warning` status is TERMINAL**, not a state to poll through. It ends the wait with
  `Result.Warned = true` and the API's message, which the caller surfaces as a **warning
  diagnostic** — a `degraded` cluster has finished coming up (§4.1). Polling on would hang
  until the timeout and then report a failure that did not happen.
- Backoff is **1 s, 2 s, 4 s, 8 s, then 15 s**, capped **after** the doubling so the
  sequence never jumps 8 s → 16 s. One sleep per non-terminal probe.

A status in `Failed` returns a `*wait.FailedError` carrying `status_message`. A timeout
returns an error **wrapping `wait.ErrTimeout`** (so `errors.Is` works) whose message
**names the last observed status**. A probe error short-circuits, wrapped so `errors.Is`
and `errors.As` still reach the cause. `wait.ForGone(ctx, probe, Options)` polls until the
read returns 404. Timeouts come from the framework `timeouts` block (defaults §5.1);
`Now`/`Sleep` are injectable so a fake clock makes it unit-testable, and the production
`Sleep` honours context cancellation.

### 3.4 Version helper

`versions.SemanticallyEqual(configured, actual string) bool`: true when both parse as
`MAJOR.MINOR[.PATCH]`, the **major and minor** both match, and either the configured value
has no patch or the patches are equal. Used as the `k8s_version` semantic-equality function so `1.37`
in HCL versus `1.37.0` from the API is not drift. `versions.IsForward(from, to)` guards
upgrades (§4.1). There is deliberately **no `latest` keyword at this layer**: a version
string here is a version, and resolving a moving target belongs to whatever chooses the
value.

**Direction is not interchangeable.** The framework calls
`proposedNew.StringSemanticEquals(ctx, prior)` and, when that returns true, keeps the
**prior** value in state (measured: terraform-plugin-framework v1.19.0
`internal/fwschemadata/value_semantic_equality_string.go:37`, and `:53`
`resp.NewValue = priorValuable`). So the **receiver is the API's value** and the
**argument is the configured/prior** one, and the hook computes
`SemanticallyEqual(configured = argument, actual = receiver)` — never the reverse:
`SemanticallyEqual("1.37", "1.37.0")` is true while `SemanticallyEqual("1.37.0", "1.37")`
is false. A hook written the wrong way round still passes a test whose two operands happen
to be ordered the same way, so the test must fix the receiver as the API value.

### 3.5 Error mapper

The API answers errors with `ErrorResponse{error{code, message, details?}}`, and a 422
carries its per-field errors in **`error.details[]{field, code, message}`**. There is **no
top-level `errors` list** and **no `request_id` in the body**: the request id is the
**`X-Request-ID` response header**, which the middleware sets on every route. Measured in
aether-api `internal/api/handler/response.go` (`ErrorBody{Code, Message, Details
[]model.ValidationError}`) and `internal/api/middleware/requestid.go` @ 8f03fbb. The
parser therefore takes the header —
`apierr.Parse(status int, header http.Header, body []byte) *apierr.Error` — and call sites
pass `resp.HTTPResponse.Header`. `apierr.ToDiagnostics` turns each
validation error into an attribute-scoped diagnostic (path from `field`), a 409 into a
conflict diagnostic on the resource, 401/403 into one provider-level message ("check
api_token / organization_id"), 404 on a **resource** Read into "remove from state", 404 on
a **data source** Read into a "not found" error. Transport: 429 is retried for every method
because it is always pre-execution — aether-api's rate limiter is a router-level middleware
mounted before any handler (`internal/api/router.go`: `r.Use(middleware.RateLimiter(…))`), so a
429 never reaches a handler and cannot have side effects. That invariant belongs to aether-api
and is guarded THERE (router-level test, aether-api follow-up issue), not by this provider —
the provider documents its reliance on it in the retry code and in the changelog for any
`OPENAPI_REF` bump. A `Retry-After` is honoured in **integer seconds only** (the HTTP-date
form is legal but this API does not send it, and guessing wrong means sleeping for hours),
**clamped to ≤ 60 s** and overflow-guarded, so a hostile or buggy value cannot park an
apply.

**5xx** is retried **only for idempotent methods** (GET/HEAD/PUT/DELETE/OPTIONS), **1 + 3
attempts**, backoff 500 ms → 1 s → 2 s capped at 4 s. POST and PATCH (`/clusters`,
`/upgrade`, `/rotate-secret`, `/registry`, robot accounts, policies) are **not** retried on
5xx: a POST that fails after the server may have acted surfaces the error, and the next
`terraform apply` reconciles through Read (a 409 on a name-based create is surfaced with
the API's message — adopting an existing resource is `terraform import`, never an automatic
merge).

A **transport error** is retried for idempotent methods. For POST/PATCH it is replayed
**only when the failure proves nothing was written** — a `*net.DNSError`, or a
`*net.OpError` with `Op == "dial"`, both of which happen before the first byte of the
request leaves. net/http exposes no "nothing written" signal of its own, so a connection
that broke **mid-body is deliberately not replayed**. Nothing is replayed at all unless the
body can be re-opened via `req.GetBody` — a replay that sends a half-consumed payload is
worse than the failure it is papering over. Every **discarded** response body is drained
**bounded (64 KiB)** and closed, so the connection is reusable without reading an unbounded
error page; the **final** response propagates unread and unclosed, exactly as if there were
no retry layer.

## 4. Resources

Ids are the API UUIDs. Every `format: uuid` field is generated as `openapi_types.UUID`
(a `[16]byte`, not a string), so provider models hold **canonical UUID text** and parse at
the **client boundary**; a malformed id is an attribute diagnostic there, before any
request is built, and never a downstream encoding failure naming neither attribute nor
value. Test fakes serve real UUID text for the same reason. Import is
`terraform import <address> <uuid>`; singletons import by
the parent id (`aether_cluster_network` by cluster id; `aether_registry`,
`aether_billing_budget`, `aether_organization` by organization id). Field names are the
live API's.

### 4.1 `aether_cluster`

Configured: `name`, `region` (**replace**), `k8s_version`, `public_endpoint` (default
`true` — owner ruling 2026-09-22: the platform currently has no supported access path
without it; may change), `plan` (optional). Computed: `id`, `status`, `status_message`, `endpoint`,
`observed_kubernetes_version`, `capi_cluster_name`, `keycloak_realm`, `harbor_project`,
`ceph_pool`, `lb_subnet`, `worker_subnet`, `created_at`, `updated_at`.

- Create: `POST /clusters` → wait until status is `running` (success) or `failed`. Live
  `ClusterStatus` = `provisioning | running | upgrading | degraded | deleting | failed`;
  `degraded` is treated as terminal-with-warning (the resource is usable; a warning
  diagnostic carries `status_message`). The same rule applies after an upgrade. PRD §7.2.5
  still lists `deleted` and omits `degraded`/`upgrading`; it is corrected in the docs wave
  (Task 17) to the live enum, and the wait helper's tests cover the warning path.
- Update: `name`/`public_endpoint` via `PUT`. A `k8s_version` change goes through
  `POST /upgrade` with `target_version`, then waits; the plan phase refuses a backwards
  change (`IsForward` false) with a diagnostic. `k8s_version` is stored as configured and
  compared with `SemanticallyEqual`; `observed_kubernetes_version` shows the live value.
- Delete: `DELETE` → `wait.ForGone`.
- A `failed` terminal state returns the API's `status_message` and keeps the resource in
  state for inspection/destroy.

### 4.2 `aether_node_pool`

Configured: `cluster_id` (**replace**), `name`, `vcpus`, `memory_gb`, `disk_gb`,
`desired_nodes`, `min_nodes`, `max_nodes`, `min_vcpus`, `max_vcpus`, `min_memory_gb`,
`max_memory_gb`, `node_labels` (**replace**), `node_taints` (**replace**; list of `{key, value, effect ∈ NoSchedule|PreferNoSchedule|NoExecute}`),
`drain_timeout_seconds`, `scale_down_delay_seconds`, `scale_down_utilization_percent`.
Computed: `id`, `current_nodes`, `status`, `status_message`, `first_ready_at`, timestamps.

- The four budget attributes are what make a pool autoscaling (`max_vcpus > min_vcpus ||
  max_memory_gb > min_memory_gb`); the schema descriptions say so and that autoscaling
  pools bill hourly per node while fixed pools bill a flat monthly fee.
- Omitted `min_nodes`/`max_nodes`/`desired_nodes` are `Optional+Computed` so the API's
  defaults (desired→1, min/max→desired) reflect back without drift. `drain_timeout_seconds`
  omitted means "track the platform default" (the API's NULL sentinel) and stays null.
- `node_labels`/`node_taints` force replacement because `UpdateNodePoolRequest` has no
  such fields.
- Create/update wait until `ready` (success) or `failed` (`NodePoolStatus` =
  `provisioning | ready | scaling | deleting | failed`); delete waits for 404.

### 4.3 `aether_cluster_network`

Singleton per cluster: `cluster_id` (**replace**), `authorized_cidrs`,
`api_server_rate_limit` (nested object: `max_connections_per_second`, `burst`). Create and Update both
`PUT`; Delete `PUT`s an empty CIDR list and default rate limit (the API has no DELETE) —
documented on the resource.

### 4.4 `aether_organization`

The token's organization. Configured: `name`, `slug`, `billing_email`, `vat_id`; computed
`plan`, timestamps. Create is **refused** ("organizations are created at registration;
import yours"). Delete is **state-only**: it succeeds and removes the resource from state
without calling the API (an organization is never deleted by Terraform), and the docs say so.
The resource exists to manage the mutable fields and always enters state via import.

### 4.5 `aether_organization_member`

`email` (**replace**), `role` (`owner|admin|member|viewer`); computed `invited_by`,
`accepted_at`, `created_at`.

### 4.6 `aether_webhook`

`url`, `description`, `events`, `enabled` (default `true`); computed `secret` (sensitive).
`rotate_secret` is an optional string attribute whose value change triggers
`POST /rotate-secret` on Update and refreshes `secret` (the "change a trigger value"
pattern).

### 4.7 `aether_registry`

Singleton per organization. Create = `POST /registry` (enable); Delete = `DELETE /registry`
(disable — the docs warn it removes the org's Harbor project). Computed: `harbor_project`,
`endpoint`, `quota_gb`, `used_gb`, `enabled`, and the enable-time robot credentials
(`robot_username`, `robot_secret`, sensitive, write-once).

### 4.8 `aether_registry_robot_account`

Named `registry_…`, not the PRD's `aether_harbor_robot_account`: the platform rule is to never
expose the underlying technology in names (same reason resources are `aether-*`, not `ceph-*`).
Intentional rename; PRD §7.2.2 is corrected in the docs wave.

`name`, `permissions`; computed `secret` (sensitive, write-once), `harbor_id`,
`last_used_at`. No update endpoint → every change forces replacement.

### 4.9 `aether_registry_cleanup_policy`

`name`, `action`, `conditions` (list of `{type, op, value}`), `repo_scope`, `schedule`,
`enabled`; computed last-run fields.

### 4.10 `aether_billing_budget`

Singleton: `monthly_limit_eur`, `thresholds_pct`. Create/Update `PUT`, Delete `DELETE`.

## 5. Data sources

`aether_organization`, `aether_regions`, `aether_k8s_versions` (`version`, `label`,
`patch`), `aether_cluster` (by `id` or by `name`), `aether_clusters`, `aether_node_pool`,
`aether_node_pools`, `aether_node_pool_machines`, `aether_cluster_network`,
`aether_kubeconfig` (`content`, sensitive — the cluster's **admin** kubeconfig, the same
artefact the portal downloads, which converges on the public endpoint; it is privileged, so the
docs say to treat the state file as a secret. PRD §7.2.4 calls it "OIDC" and is corrected in the
docs wave), `aether_loadbalancers`, `aether_cluster_volumes`,
`aether_organization_members`, `aether_webhooks`, `aether_registry`,
`aether_registry_repositories`, `aether_registry_repository_tags`,
`aether_registry_robot_accounts`, `aether_registry_cleanup_policies`, `aether_audit_log`,
`aether_usage`, `aether_cost_breakdown`, `aether_invoices`.

List sources page through `?cursor=&limit=` to exhaustion. Lookup by `name` errors on zero
or multiple matches. Nothing with side effects (SSE streams, scan triggers, webhook tests,
policy runs, invoice PDFs) is exposed.

### 5.1 Timeouts

Framework `timeouts` block on cluster and node pool: create 30 m, update 30 m, delete 20 m.

## 6. Error handling summary

| Situation | Behaviour |
|---|---|
| 422 validation | attribute-scoped diagnostics from `error.details[].field` |
| 409 conflict | resource-level diagnostic with the API message |
| 401 / 403 | one provider-level diagnostic: check `api_token` / `organization_id` |
| 404 on a resource Read | remove from state (also the delete-completion signal) |
| 404 on a data source Read | error "not found" (never silently empty) |
| 429 | retry (every method) honouring integer-seconds `Retry-After`, clamped to ≤ 60 s |
| 5xx | 3 retries with backoff for idempotent methods only; POST/PATCH surface at once |
| transport | 3 retries for idempotent methods; POST/PATCH only when nothing was written |
| any of the above | surfaced with the request id from the `X-Request-ID` response header |
| terminal `failed` | error with `status_message`; resource stays in state |
| wait timeout | error naming the last observed status; resource stays in state |

## 7. Testing

1. **Unit (every PR, no network):** an `httptest` fake API per resource playing recorded
   response shapes; asserts request bodies, the version and error helpers, and the wait
   helper with a fake clock (terminal, failed-with-message, timeout, 404-after-delete).
   Written by a separate test-writer before the resource is implemented.
2. **Acceptance (dev, explicit):** `terraform-plugin-testing` `resource.Test` gated on
   `TF_ACC=1`, `AETHER_ENDPOINT=https://dev.api.aetherplatform.cloud`, `AETHER_API_TOKEN`
   from a dedicated dev organization the owner creates in the portal (repo secret for the
   manual/nightly workflow). Serial; one-node pools with the smallest sizing; always
   destroys, even on failure. Scenarios: organization import+update; cluster + pool
   create → resize → upgrade one minor → network CIDRs → kubeconfig used against the API
   server → destroy → 404; member, webhook (+rotation), registry enable, robot account,
   cleanup policy, budget round-trips.
3. **Contract:** a test asserts `OPENAPI_REF` matches the vendored spec and that every
   schema field the provider maps exists, so an api rename fails our CI, not a customer.
4. **Both engines:** unit and acceptance suites run twice — with `terraform` and with
   `tofu` (`TF_ACC_TERRAFORM_PATH`, `TF_ACC_PROVIDER_HOST=registry.opentofu.org`).

## 8. Compatibility, CI, release, docs

- **Terraform and OpenTofu:** plugin protocol v6 → Terraform 1.x and OpenTofu ≥ 1.6. No
  engine-specific features (no ephemeral/write-only attributes until both support them).
  Docs and examples show `terraform` and `tofu` invocations and the version floors.
- **CI (Actions):** build, `go vet`, unit tests, `go generate` diff check, `tfplugindocs`
  drift check on every PR; CodeRabbit required. Acceptance workflow is manual/nightly only.
- **Release:** tags `v0.x.y` → GoReleaser (registry platform matrix, GPG-signed
  `SHA256SUMS`, `terraform-registry-manifest.json`) → GitHub Release. Then the owner
  flips the repo public, registers `plane-shift/aether` on the Terraform Registry (GPG
  public key) and submits the OpenTofu Registry manifest + key. Image publishing does not
  apply (no container).
- **Docs:** `tfplugindocs` generates `docs/`; aether-docs gets a Terraform/OpenTofu section
  linking to the registry docs plus a getting-started page whose step 1 is "create an API
  token in the portal"; PRD §7.2 corrected to the live field names in the same wave.

## 9. Open items for the owner

- Create the dedicated dev organization + API token for acceptance tests (portal).
- Generate the release GPG key (kept by the owner) and register it with both registries.
- Decide when to flip the repo public (before the first registry publish).
