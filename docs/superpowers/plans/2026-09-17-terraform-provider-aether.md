# terraform-provider-aether Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Terraform + OpenTofu provider `plane-shift/aether` covering every tenant-manageable Aether API resource (cluster, node pools, network, organization, members, webhooks, registry, robot accounts, cleanup policies, billing budget) and every read-only view as a data source, with unit tests on every PR and acceptance tests against dev.

**Architecture:** `terraform-plugin-framework` provider over an `oapi-codegen`-generated client vendored from `aether-api/api/openapi.yaml` at a pinned commit. Hand-written per-resource files carry all judgement (poll-to-terminal `wait`, `k8s_version` semantic equality, error→diagnostic mapping). Organization is provider-level, resolved from the API token.

**Tech Stack:** Go 1.26.5; `github.com/hashicorp/terraform-plugin-framework v1.19.0`, `terraform-plugin-framework-timeouts v0.7.0`, `terraform-plugin-testing v1.16.0`, `terraform-plugin-go v0.31.0`, `terraform-plugin-log v0.11.0`, `terraform-plugin-docs v0.25.0`, `github.com/oapi-codegen/oapi-codegen/v2 v2.8.0`; GoReleaser; GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-09-17-terraform-provider-aether-design.md` (this plan argues from it; executors read both).

## Global Constraints

- Module path `github.com/plane-shift/terraform-provider-aether`; provider type name `aether`; registry address `plane-shift/aether`; plugin protocol **v6**; supports Terraform 1.x and OpenTofu ≥ 1.6 — no engine-specific features (no ephemeral / write-only attributes).
- Go `1.26.5` in `go.mod`. Library versions exactly as in Tech Stack (bump only via Dependabot PRs).
- `internal/client/` is **generated only** (`go generate ./...`); CI fails on a generate diff. The vendored spec `api/openapi.yaml` is never hand-edited; `OPENAPI_REF` holds the full aether-api commit SHA it came from (initial: `03459f8f0c9edd982550e9f6ab3292bab5bb96c7`).
- Organization is **provider-level**: resources carry NO `organization_id`. API tokens are created in the portal; there is NO `aether_api_token` resource.
- Out of scope: payment methods, tickets, admin routes, SSE streams, scan triggers, webhook tests, policy runs, invoice PDFs.
- Field names are the live API's (`vcpus`, `memory_gb`, `disk_gb`, `min_vcpus`…), never the PRD example's (`machine_tier`, `eu-de`).
- `k8s_version` semantic equality: `1.37` ≡ `1.37.0`; a backwards `k8s_version` change is refused at plan time; forward changes go through `POST …/upgrade`.
- Terminal statuses: cluster `running` (success), `degraded` (success + warning), `failed` (error); node pool `ready` (success), `failed` (error). Delete waits for HTTP 404. Default timeouts: create 30m, update 30m, delete 20m.
- Unit tests never touch the network (`httptest`); acceptance tests run only with `TF_ACC=1` against `AETHER_ENDPOINT` (dev) and are serial, one-node pools, always destroy.
- Tests are written by a separate test-writer agent before the code; coding agents never modify `_test.go` files.
- Every commit ends with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Branch + PR (`protect_main`; CodeRabbit required); pre-push loop = `code-review` + `coderabbit:code-review`, max 3 rounds.
- Scratch files only under repo-local `.tmp/` (gitignored). Never `/tmp`, never `~`.

---

## File Structure

```
go.mod / go.sum
main.go                                   # providerserver.Serve, -debug flag
tools.go                                  # tool deps (oapi-codegen, tfplugindocs) behind //go:build tools
Makefile                                  # build, test, testacc, generate, docs, openapi targets
OPENAPI_REF                               # full aether-api SHA
api/openapi.yaml                          # vendored spec
scripts/fetch-openapi.sh                  # curl the spec at $OPENAPI_REF via gh api (private until public flip)
internal/client/oapi-codegen.yaml         # config
internal/client/generate.go               # //go:generate directive
internal/client/client.gen.go             # GENERATED
internal/provider/provider.go             # schema, Configure, org resolution, resource/data source registries
internal/provider/provider_test.go        # unit: configure paths
internal/provider/apierr/apierr.go        # ErrorResponse/ValidationError parsing → diagnostics
internal/provider/apierr/apierr_test.go
internal/provider/transport/retry.go      # 429/5xx retrying http.RoundTripper + bearer auth
internal/provider/transport/retry_test.go
internal/provider/wait/wait.go            # ForStatus / ForGone with injectable clock
internal/provider/wait/wait_test.go
internal/provider/versions/versions.go    # K8sVersionType/Value (semantic equals), IsForward
internal/provider/versions/versions_test.go
internal/provider/fakeapi/fakeapi.go      # httptest fake Aether API used by unit tests (org, clusters, pools, …)
internal/provider/resource_cluster.go (+_test.go)
internal/provider/resource_node_pool.go (+_test.go)
internal/provider/resource_cluster_network.go (+_test.go)
internal/provider/resource_organization.go (+_test.go)
internal/provider/resource_organization_member.go (+_test.go)
internal/provider/resource_webhook.go (+_test.go)
internal/provider/resource_registry.go (+_test.go)
internal/provider/resource_registry_robot_account.go (+_test.go)
internal/provider/resource_registry_cleanup_policy.go (+_test.go)
internal/provider/resource_billing_budget.go (+_test.go)
internal/provider/datasource_*.go (+_test.go)   # one per data source (§Task 13/14 tables)
internal/provider/paging.go               # cursor pagination helper
internal/acctest/acctest.go               # PreCheck, provider factories, env
internal/acctest/*_test.go                # acceptance scenarios (TF_ACC)
examples/provider/provider.tf, examples/resources/<name>/resource.tf, examples/data-sources/<name>/data-source.tf
templates/index.md.tmpl
docs/                                     # tfplugindocs output (committed)
.goreleaser.yml, terraform-registry-manifest.json
.github/workflows/build-test.yml, release.yml, acceptance.yml, dependabot.yml (exists)
```

Generated client naming (oapi-codegen, from operationIds): types `client.Cluster`, `client.CreateClusterRequest`, …; request bodies `client.CreateClusterJSONRequestBody` (= alias of the request type); methods on `*client.ClientWithResponses` named `<OperationId>WithResponse`, e.g. `GetClusterWithResponse(ctx, orgID, clusterID)`, `CreateClusterWithResponse(ctx, orgID, body)`, `ListClustersWithResponse(ctx, orgID, &client.ListClustersParams{Cursor: …, Limit: …})`. Responses expose `StatusCode()`, `Body []byte`, `HTTPResponse *http.Response` (the source of the `X-Request-ID` header `apierr.Parse` needs) and `JSON200 *client.Cluster`-style typed fields. **Implementers verify exact names in `client.gen.go` after Task 2 and use those.**

**IDs are `openapi_types.UUID`, not strings.** Every `format: uuid` field in the spec is generated as `openapi_types.UUID` (an alias of `google/uuid.UUID`, a `[16]byte`) — path parameters (`client.OrgID`, `client.ClusterID`, `client.PoolID`, …) and struct fields (`Organization.Id`) alike. So: **provider models and `providerData` hold canonical UUID TEXT (`string`), and the parse happens at the client boundary** — `var u client.OrgID; u.UnmarshalText([]byte(s))`, then `u.String()` to canonicalise. A malformed id is an **attribute diagnostic raised before any request is built**, never a downstream encoding failure that names neither the attribute nor the value. Parsing via the generated alias's own `UnmarshalText` keeps `github.com/google/uuid` an INDIRECT dependency. Test fakes serve real UUID text for the same reason — a fake keyed on `"org-1"` exercises a path production never takes.

---

### Task 1: Module scaffold, provider skeleton, Configure with organization resolution

**Files:**
- Create: `go.mod`, `main.go`, `tools.go`, `Makefile`, `.gitignore`, `internal/provider/provider.go`, `internal/provider/fakeapi/fakeapi.go`
- Test: `internal/provider/provider_test.go`

**Interfaces:**
- Produces: `provider.New(version string) func() provider.Provider`; `type providerData struct { Client *client.ClientWithResponses; OrgID string }` stored in `resp.ResourceData`/`resp.DataSourceData`; `fakeapi.New(t) *fakeapi.Server` with `URL()`, `Token()`, `AddOrg(id, name, slug)`; env vars `AETHER_API_TOKEN`, `AETHER_ENDPOINT`, `AETHER_ORGANIZATION_ID`.
- `github.com/oapi-codegen/runtime` **v1.7.0** is a required (direct) dependency — the generated client imports it.
- **`providerData.OrgID` stays a `string`** holding canonical UUID text, because every resource carries it as configuration data; the generated client wants `client.OrgID` (= `openapi_types.UUID`), so `Configure` parses at the boundary. An explicit `organization_id` that is not a UUID is an **attribute error on `organization_id` raised BEFORE any API call** — not a 404 from a request that could never have been well-formed. `fakeapi` therefore registers real UUID text, and the tests assert on it.
- Consumes: `internal/client` from Task 2 (Task 1 and 2 are executed together in one branch; Task 1's `Configure` compiles only after Task 2's generation — run Task 2 Steps 1–4 first if the tree does not compile, then return).

- [ ] **Step 1 [CODER]: Scaffold**

```bash
cd /Users/zozo/k8s/aether/terraform-provider-aether
git checkout -b feat/provider-scaffold origin/main
go mod init github.com/plane-shift/terraform-provider-aether
go get github.com/hashicorp/terraform-plugin-framework@v1.19.0 \
       github.com/hashicorp/terraform-plugin-framework-timeouts@v0.7.0 \
       github.com/hashicorp/terraform-plugin-go@v0.31.0 \
       github.com/hashicorp/terraform-plugin-log@v0.11.0 \
       github.com/hashicorp/terraform-plugin-testing@v1.16.0 \
       github.com/oapi-codegen/oapi-codegen/v2@v2.8.0 \
       github.com/oapi-codegen/runtime@v1.7.0 \
       github.com/hashicorp/terraform-plugin-docs@v0.25.0
printf '.tmp/\n.worktrees/\ndist/\n*.test\nterraform-provider-aether\n' > .gitignore
```

`main.go`:

```go
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/plane-shift/terraform-provider-aether/internal/provider"
)

// version is set by GoReleaser (-X main.version=…); "dev" for local builds.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers (delve)")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/plane-shift/aether",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

`tools.go`:

```go
//go:build tools

package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
)
```

`Makefile`:

```make
.PHONY: build test testacc generate docs openapi lint
build:      ; go build ./...
test:       ; go test ./... -count=1 -race
testacc:    ; TF_ACC=1 go test ./internal/acctest/... -count=1 -v -timeout 120m
generate:   ; go generate ./...
docs:       ; go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-name aether
openapi:    ; ./scripts/fetch-openapi.sh && $(MAKE) generate
lint:       ; go vet ./... && test -z "$$(gofmt -l .)"
```

- [ ] **Step 2 [TEST-WRITER]: Write the failing provider tests**

`internal/provider/fakeapi/fakeapi.go` (test support; lives outside `_test.go` so every package's tests can use it — the test-writer owns it):

```go
// Package fakeapi is an in-process fake of the Aether API for unit tests.
// It speaks exactly the JSON shapes of api/openapi.yaml and records requests.
package fakeapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type Org struct {
	ID, Name, Slug string
}

type Request struct {
	Method, Path string
	Body         []byte
}

type Server struct {
	t     testing.TB
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	orgs     []Org
	requests []Request
	// Handlers lets a test override a route: key "METHOD /path" (path after /api/v1).
	Handlers map[string]http.HandlerFunc
}

func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, token: "aether_" + strings.Repeat("a", 64), Handlers: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *Server) URL() string   { return s.srv.URL }
func (s *Server) Token() string { return s.token }

func (s *Server) AddOrg(id, name, slug string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orgs = append(s.orgs, Org{ID: id, Name: name, Slug: slug})
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "invalid token"}})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	var body []byte
	if r.Body != nil {
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		body = buf[:n]
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: path, Body: body})
	h := s.Handlers[r.Method+" "+path]
	s.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/organizations/") && strings.Count(path, "/") == 2:
		id := strings.TrimPrefix(path, "/organizations/")
		s.mu.Lock()
		var found *Org
		for i := range s.orgs {
			if s.orgs[i].ID == id {
				found = &s.orgs[i]
			}
		}
		s.mu.Unlock()
		if found == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "organization not found"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": found.ID, "name": found.Name, "slug": found.Slug, "plan": "free",
			"created_at": "2026-09-17T00:00:00Z", "updated_at": "2026-09-17T00:00:00Z"})
	case r.Method == http.MethodGet && path == "/organizations":
		s.mu.Lock()
		items := make([]map[string]any, 0, len(s.orgs))
		for _, o := range s.orgs {
			items = append(items, map[string]any{"id": o.ID, "name": o.Name, "slug": o.Slug, "plan": "free",
				"created_at": "2026-09-17T00:00:00Z", "updated_at": "2026-09-17T00:00:00Z"})
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "no route " + r.Method + " " + path}})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteJSON is exported for test handlers.
func WriteJSON(w http.ResponseWriter, code int, v any) { writeJSON(w, code, v) }
```

`internal/provider/provider_test.go`:

```go
package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// Every organization id the generator produces is openapi_types.UUID, and
// providerData.OrgID holds the canonical text the client round-trips it as.
// These are real UUIDs rather than opaque labels, so the fixtures exercise the
// same parse/format path production traffic does.
const (
	orgAcme      = "11111111-1111-4111-8111-111111111111"
	orgSecondary = "22222222-2222-4222-8222-222222222222"
	orgUnseen    = "33333333-3333-4333-8333-333333333333" // never registered on the fake
)

func configure(t *testing.T, endpoint, token, orgID string) (provider.ConfigureResponse, *providerData) {
	t.Helper()
	p := New("test")()
	schemaResp := provider.SchemaResponse{}
	p.Schema(context.Background(), provider.SchemaRequest{}, &schemaResp)
	vals := map[string]tftypes.Value{
		"api_token":       tftypes.NewValue(tftypes.String, token),
		"endpoint":        tftypes.NewValue(tftypes.String, endpoint),
		"organization_id": tftypes.NewValue(tftypes.String, nil),
	}
	if orgID != "" {
		vals["organization_id"] = tftypes.NewValue(tftypes.String, orgID)
	}
	raw := tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"api_token": tftypes.String, "endpoint": tftypes.String, "organization_id": tftypes.String,
	}}, vals)
	req := provider.ConfigureRequest{Config: tfsdk.Config{Raw: raw, Schema: schemaResp.Schema}}
	resp := provider.ConfigureResponse{}
	p.Configure(context.Background(), req, &resp)
	pd, _ := resp.ResourceData.(*providerData)
	return resp, pd
}

func TestConfigure_ResolvesTheSingleOrganization(t *testing.T) {
	api := fakeapi.New(t)
	api.AddOrg(orgAcme, "Acme", "acme")
	resp, pd := configure(t, api.URL(), api.Token(), "")
	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
	if pd == nil || pd.OrgID != orgAcme {
		t.Fatalf("OrgID = %+v, want %s", pd, orgAcme)
	}
}

func TestConfigure_ExplicitOrganizationIsValidatedNotListed(t *testing.T) {
	api := fakeapi.New(t)
	api.AddOrg(orgAcme, "Acme", "acme")
	resp, pd := configure(t, api.URL(), api.Token(), orgAcme)
	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
	if pd.OrgID != orgAcme {
		t.Fatalf("OrgID = %q", pd.OrgID)
	}
	var listed, validated bool
	for _, r := range api.Requests() {
		if r.Path == "/organizations" {
			listed = true
		}
		if r.Path == "/organizations/"+orgAcme {
			validated = true
		}
	}
	if listed || !validated {
		t.Fatalf("listed=%v validated=%v; want GET /organizations/{id} only", listed, validated)
	}
}

func TestConfigure_ExplicitOrganizationTheTokenCannotSeeIsRejected(t *testing.T) {
	api := fakeapi.New(t)
	api.AddOrg(orgAcme, "Acme", "acme")
	resp, _ := configure(t, api.URL(), api.Token(), orgUnseen) // fake answers 404 for unregistered orgs
	if !resp.Diagnostics.HasError() || !contains(resp.Diagnostics.Errors()[0].Detail(), "organization_id") {
		t.Fatalf("expected an organization_id error, got %v", resp.Diagnostics)
	}
}

// An organization_id that is not a UUID cannot even be turned into a request
// path, so it must be an attribute error BEFORE any API call - never a
// downstream encoding failure naming neither the attribute nor the value.
func TestConfigure_ExplicitOrganizationMustBeAUUID(t *testing.T) {
	api := fakeapi.New(t)
	api.AddOrg(orgAcme, "Acme", "acme")
	resp, _ := configure(t, api.URL(), api.Token(), "not-a-uuid")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an organization_id error")
	}
	if got := resp.Diagnostics.Errors()[0].Detail(); !contains(got, "organization_id") {
		t.Fatalf("detail = %q, want a hint naming organization_id", got)
	}
	for _, r := range api.Requests() {
		t.Fatalf("expected no API call for an invalid organization_id, got %s %s", r.Method, r.Path)
	}
}

func TestConfigure_ErrorsWhenTokenSeesSeveralOrganizations(t *testing.T) {
	api := fakeapi.New(t)
	api.AddOrg(orgAcme, "A", "a")
	api.AddOrg(orgSecondary, "B", "b")
	resp, _ := configure(t, api.URL(), api.Token(), "")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error asking for organization_id")
	}
	if got := resp.Diagnostics.Errors()[0].Detail(); !contains(got, "2 organizations") || !contains(got, "organization_id") {
		t.Fatalf("detail = %q", got)
	}
}

func TestConfigure_ErrorsWhenTokenSeesNoOrganization(t *testing.T) {
	api := fakeapi.New(t)
	resp, _ := configure(t, api.URL(), api.Token(), "")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error")
	}
}

func TestConfigure_BadTokenIsOneProviderLevelError(t *testing.T) {
	api := fakeapi.New(t)
	api.AddOrg(orgAcme, "A", "a")
	resp, _ := configure(t, api.URL(), "aether_wrong", "")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error")
	}
	if got := resp.Diagnostics.Errors()[0].Detail(); !contains(got, "api_token") {
		t.Fatalf("detail = %q, want a hint naming api_token", got)
	}
}

func TestConfigure_ReadsEnvironment(t *testing.T) {
	api := fakeapi.New(t)
	api.AddOrg(orgAcme, "A", "a")
	t.Setenv("AETHER_API_TOKEN", api.Token())
	t.Setenv("AETHER_ENDPOINT", api.URL())
	p := New("test")()
	schemaResp := provider.SchemaResponse{}
	p.Schema(context.Background(), provider.SchemaRequest{}, &schemaResp)
	raw := tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"api_token": tftypes.String, "endpoint": tftypes.String, "organization_id": tftypes.String,
	}}, map[string]tftypes.Value{
		"api_token": tftypes.NewValue(tftypes.String, nil), "endpoint": tftypes.NewValue(tftypes.String, nil), "organization_id": tftypes.NewValue(tftypes.String, nil),
	})
	resp := provider.ConfigureResponse{}
	p.Configure(context.Background(), provider.ConfigureRequest{Config: tfsdk.Config{Raw: raw, Schema: schemaResp.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 3 [TEST-RUNNER]: Confirm RED**

Run: `go test ./internal/provider/ -run TestConfigure -count=1`
Expected: build failure — `New`, `providerData` undefined.

- [ ] **Step 4 [CODER]: Implement `provider.go`**

```go
package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/transport"
)

const defaultEndpoint = "https://api.aetherplatform.cloud"

// providerData is what every resource and data source receives from Configure.
type providerData struct {
	Client *client.ClientWithResponses
	OrgID  string
}

type aetherProvider struct{ version string }

type providerModel struct {
	APIToken       types.String `tfsdk:"api_token"`
	Endpoint       types.String `tfsdk:"endpoint"`
	OrganizationID types.String `tfsdk:"organization_id"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &aetherProvider{version: version} }
}

func (p *aetherProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "aether"
	resp.Version = p.version
}

func (p *aetherProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage Aether Kubernetes clusters, access and registry. Create an API token in the Aether portal first; the token belongs to one organization, which the provider uses for every resource.",
		Attributes: map[string]schema.Attribute{
			"api_token": schema.StringAttribute{
				Optional: true, Sensitive: true,
				MarkdownDescription: "API token created in the portal (`aether_…`). Falls back to `AETHER_API_TOKEN`.",
			},
			"endpoint": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "API base URL. Defaults to `" + defaultEndpoint + "`; falls back to `AETHER_ENDPOINT`.",
			},
			"organization_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Organization the token belongs to. Optional: resolved automatically when the token sees exactly one organization. Falls back to `AETHER_ORGANIZATION_ID`.",
			},
		},
	}
}

func (p *aetherProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	token := firstNonEmpty(cfg.APIToken.ValueString(), os.Getenv("AETHER_API_TOKEN"))
	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv("AETHER_ENDPOINT"), defaultEndpoint)
	orgID := firstNonEmpty(cfg.OrganizationID.ValueString(), os.Getenv("AETHER_ORGANIZATION_ID"))
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("api_token"), "Missing API token",
			"Set api_token in the provider block or the AETHER_API_TOKEN environment variable. Tokens are created in the Aether portal.")
		return
	}

	// The generated client types organization_id as client.OrgID
	// (= openapi_types.UUID), so parse at the boundary, BEFORE any call: a
	// syntax error belongs on the attribute, not in a downstream encoding
	// failure. providerData.OrgID stays canonical UUID TEXT.
	var orgUUID client.OrgID
	if orgID != "" {
		if err := orgUUID.UnmarshalText([]byte(orgID)); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("organization_id"), "Invalid organization_id",
				fmt.Sprintf("organization_id must be a UUID: %q is not (%s). Copy the id from the Aether portal, or omit organization_id and the provider resolves the organization the token belongs to.", orgID, err))
			return
		}
		orgID = orgUUID.String() // canonicalise
	}

	httpClient := &http.Client{Transport: transport.New(token, http.DefaultTransport)}
	c, err := client.NewClientWithResponses(endpoint, client.WithHTTPClient(httpClient))
	if err != nil {
		resp.Diagnostics.AddError("Cannot build API client", err.Error())
		return
	}

	if orgID != "" {
		// Validate the override: the token belongs to exactly one organization.
		g, err := c.GetOrganizationWithResponse(ctx, orgUUID)
		if err != nil {
			resp.Diagnostics.AddError("Cannot reach the Aether API", fmt.Sprintf("GET %s/api/v1/organizations/%s: %s", endpoint, orgID, err))
			return
		}
		if g.StatusCode() != http.StatusOK {
			resp.Diagnostics.AddAttributeError(path.Root("organization_id"), "organization_id is not visible to this token",
				fmt.Sprintf("The API answered %s for organization %s. The token belongs to one organization; omit organization_id to use it.", g.Status(), orgID))
			return
		}
	} else {
		list, err := c.ListOrganizationsWithResponse(ctx, nil)
		if err != nil {
			resp.Diagnostics.AddError("Cannot reach the Aether API", fmt.Sprintf("GET %s/api/v1/organizations: %s", endpoint, err))
			return
		}
		switch list.StatusCode() {
		case http.StatusOK:
		case http.StatusUnauthorized, http.StatusForbidden:
			resp.Diagnostics.AddError("Aether API rejected the token",
				"The API answered "+list.Status()+". Check api_token (created in the portal) and, if set, organization_id.")
			return
		default:
			resp.Diagnostics.AddError("Cannot list organizations", list.Status()+": "+string(list.Body))
			return
		}
		items := list.JSON200.Items
		if len(items) != 1 {
			resp.Diagnostics.AddAttributeError(path.Root("organization_id"), "Cannot resolve the organization",
				fmt.Sprintf("The token sees %d organizations; set organization_id explicitly.", len(items)))
			return
		}
		orgID = items[0].Id.String()
	}

	data := &providerData{Client: c, OrgID: orgID}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *aetherProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{} // filled task by task
}

func (p *aetherProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{} // filled task by task
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// fromProviderData is the shared Configure body of every resource/data source.
func fromProviderData(raw any, diags interface{ AddError(string, string) }) *providerData {
	if raw == nil {
		return nil
	}
	pd, ok := raw.(*providerData)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("got %T", raw))
		return nil
	}
	return pd
}
```

`transport.New` is Task 3; for this task use a minimal `internal/provider/transport/retry.go` that only adds the bearer header (Task 3 replaces its body, keeping the signature `New(token string, next http.RoundTripper) http.RoundTripper`).

- [ ] **Step 5 [TEST-RUNNER]: GREEN** — `go test ./internal/provider/ -run TestConfigure -count=1 -v`; `go vet ./...`; `gofmt -l .` empty.

- [ ] **Step 6 [CODER]: Commit** — `git add -A && git commit -m "feat: provider scaffold; Configure resolves the token's organization"` (+ trailer).

---

### Task 2: Vendored OpenAPI + generated client + contract test

**Files:**
- Create: `OPENAPI_REF`, `api/openapi.yaml`, `scripts/fetch-openapi.sh`, `internal/client/oapi-codegen.yaml`, `internal/client/generate.go`, `internal/client/client.gen.go` (generated)
- Test: `internal/client/contract_test.go`

**Interfaces:** Produces the `client` package (see naming above). Consumed by everything.

- [ ] **Step 1 [CODER]: Vendoring script**

`scripts/fetch-openapi.sh`:

```bash
#!/usr/bin/env bash
# Re-vendor api/openapi.yaml from plane-shift/aether-api at the SHA in OPENAPI_REF.
# Usage: scripts/fetch-openapi.sh [<full-sha>]   (writes OPENAPI_REF when a sha is given)
set -euo pipefail
cd "$(dirname "$0")/.."
if [ "${1:-}" != "" ]; then
  [ ${#1} -eq 40 ] || { echo "need a full 40-char sha" >&2; exit 1; }
  printf '%s\n' "$1" > OPENAPI_REF
fi
REF=$(tr -d '[:space:]' < OPENAPI_REF)
[ ${#REF} -eq 40 ] || { echo "OPENAPI_REF must hold a full sha" >&2; exit 1; }
mkdir -p api
# gh api works for the private repo (token) and the public one alike.
gh api -H 'Accept: application/vnd.github.raw' "repos/plane-shift/aether-api/contents/api/openapi.yaml?ref=$REF" > api/openapi.yaml.new
python3 -c "import yaml,sys; yaml.safe_load(open('api/openapi.yaml.new'))" || { rm -f api/openapi.yaml.new; echo "fetched spec does not parse" >&2; exit 1; }
mv api/openapi.yaml.new api/openapi.yaml
echo "vendored api/openapi.yaml at $REF"
```

```bash
chmod +x scripts/fetch-openapi.sh
./scripts/fetch-openapi.sh 03459f8f0c9edd982550e9f6ab3292bab5bb96c7
```

- [ ] **Step 2 [CODER]: Generator config**

`internal/client/oapi-codegen.yaml`:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/oapi-codegen/oapi-codegen/v2.8.0/configuration-schema.json
package: client
output: client.gen.go
generate:
  models: true
  client: true
output-options:
  skip-prune: false
```

`internal/client/generate.go`:

```go
// Package client is GENERATED from ../../api/openapi.yaml. Do not edit; run `go generate ./...`.
package client

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config oapi-codegen.yaml ../../api/openapi.yaml
```

Run `go generate ./... && go build ./...`. If oapi-codegen rejects an OpenAPI 3.1 construct, record the exact error in the report; the fix goes to aether-api's spec (a PR there), never to the vendored copy.

- [ ] **Step 3 [TEST-WRITER]: Contract test**

`internal/client/contract_test.go`:

```go
package client

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// The provider maps these fields by hand; a rename upstream must fail here, not at a customer.
func TestContract_FieldsTheProviderMapsExist(t *testing.T) {
	want := map[string][]string{
		"Cluster":         {"Id", "Name", "Region", "K8sVersion", "Status", "StatusMessage", "Endpoint", "ObservedKubernetesVersion", "CapiClusterName", "KeycloakRealm", "HarborProject", "CephPool", "LbSubnet", "WorkerSubnet", "PublicEndpoint", "PlanSlug", "CreatedAt", "UpdatedAt"},
		"NodePool":        {"Id", "ClusterId", "Name", "Vcpus", "MemoryGb", "DiskGb", "MinNodes", "MaxNodes", "DesiredNodes", "MinVcpus", "MaxVcpus", "MinMemoryGb", "MaxMemoryGb", "NodeLabels", "NodeTaints", "DrainTimeoutSeconds", "ScaleDownDelaySeconds", "ScaleDownUtilizationPercent", "CurrentNodes", "FirstReadyAt", "Status", "StatusMessage"},
		"ClusterNetwork":  {"ClusterId", "AuthorizedCidrs", "ApiServerRateLimit", "UpdatedAt"},
		"Organization":    {"Id", "Name", "Slug", "BillingEmail", "VatId", "Plan"},
		"Member":          {"Id", "Email", "Role", "InvitedBy", "AcceptedAt", "CreatedAt"},
		"Webhook":         {"Id", "Url", "Description", "Events", "Enabled", "Secret"},
		"Registry":        {"Id", "HarborProject", "Endpoint", "Enabled", "QuotaGb", "UsedGb"},
		"RobotAccount":    {"Id", "Name", "Permissions", "HarborId", "LastUsedAt"},
		"CleanupPolicy":   {"Id", "Name", "Action", "Conditions", "RepoScope", "Schedule", "Enabled", "LastRunAt", "LastRunDeleted", "LastRunFreedBytes", "LastRunMessage"},
		"Budget":          {"MonthlyLimitEur", "ThresholdsPct"},
		"K8sVersionInfo":  {"Version", "Label", "Patch"},
		"Machine":         {"Name", "Status", "K8sVersion", "InternalIp", "AgeSeconds"},
		"LoadBalancer":    {"ServiceName", "Namespace", "ExternalIp", "DnsName", "Ports"},
	}
	types := map[string]reflect.Type{
		"Cluster": reflect.TypeOf(Cluster{}), "NodePool": reflect.TypeOf(NodePool{}), "ClusterNetwork": reflect.TypeOf(ClusterNetwork{}),
		"Organization": reflect.TypeOf(Organization{}), "Member": reflect.TypeOf(Member{}), "Webhook": reflect.TypeOf(Webhook{}),
		"Registry": reflect.TypeOf(Registry{}), "RobotAccount": reflect.TypeOf(RobotAccount{}), "CleanupPolicy": reflect.TypeOf(CleanupPolicy{}),
		"Budget": reflect.TypeOf(Budget{}), "K8sVersionInfo": reflect.TypeOf(K8sVersionInfo{}), "Machine": reflect.TypeOf(Machine{}), "LoadBalancer": reflect.TypeOf(LoadBalancer{}),
	}
	for name, fields := range want {
		rt := types[name]
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
	if len(ref) != 40 {
		t.Fatalf("OPENAPI_REF = %q, want a full 40-char sha", ref)
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
```

- [ ] **Step 4 [TEST-RUNNER]:** `go test ./internal/client/ -count=1 -v` → PASS (field/method names come from the generator; if one is spelled differently than the test expects, the TEST-WRITER adjusts the expectation to the generated spelling and records it — the generator, not the test, is authoritative on spelling).

- [ ] **Step 5 [CODER]: Commit** — `git add OPENAPI_REF api scripts internal/client && git commit -m "feat(client): vendor aether-api OpenAPI at 03459f8 and generate the client"`.

---

### Task 3: Error mapper and retrying transport

**Files:**
- Create: `internal/provider/apierr/apierr.go`, `internal/provider/transport/retry.go`
- Test: `internal/provider/apierr/apierr_test.go`, `internal/provider/transport/retry_test.go`

**Interfaces:**
- Produces: `apierr.Parse(status int, header http.Header, body []byte) *apierr.Error` (`Error{Status int; Code, Message, RequestID string; Fields []FieldError{Field, Code, Message}}`, implements `error`); `apierr.IsNotFound(err error) bool`; `apierr.ToDiagnostics(err error, diags *diag.Diagnostics, attrPrefix path.Path)`; `transport.New(token string, next http.RoundTripper) http.RoundTripper`.
- **The wire format is measured, not assumed** (aether-api `internal/api/handler/response.go` + `internal/api/middleware/requestid.go` @ `8f03fbb`): the envelope is `{"error":{"code","message","details":[{"field","code","message"}]}}`. Field errors live under **`error.details`**; there is **no top-level `errors` list** and **no `request_id` in the body** — the request id is the **`X-Request-ID` response header**, set by middleware on every route. That is why `Parse` takes the header; call sites pass `resp.HTTPResponse.Header`.
- Retry rules (`transport`): **429 retried for every method**, honouring **integer-seconds** `Retry-After` **clamped to ≤ 60 s** (overflow-guarded). **5xx retried only for idempotent methods** (GET/HEAD/PUT/DELETE/OPTIONS), **1 + 3 attempts**, backoff 500 ms → 1 s → 2 s capped at 4 s; POST/PATCH surface a 5xx at once. A **transport error** is retried for idempotent methods, and for POST/PATCH **only when the failure proves nothing was written** (`*net.DNSError`, or `*net.OpError{Op:"dial"}`) — a connection that broke **mid-body is never replayed**. Nothing is replayed unless the body can be re-opened via `req.GetBody`. Discarded bodies are drained **bounded (64 KiB)** and closed; the **final** response propagates unread.

- [ ] **Step 1 [TEST-WRITER]: Tests**

`internal/provider/apierr/apierr_test.go`:

```go
package apierr

import (
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

func TestParse_ValidationErrorsCarryFields(t *testing.T) {
	// Measured shape: details under error.details, request id in the header.
	body := []byte(`{"error":{"code":"validation_error","message":"invalid request","details":[{"field":"k8s_version","code":"not_offered","message":"1.31 is not offered"},{"field":"name","code":"required","message":"name is required"}]}}`)
	hdr := http.Header{}
	hdr.Set("X-Request-Id", "req-1")
	e := Parse(http.StatusUnprocessableEntity, hdr, body)
	if e.Status != 422 || e.Code != "validation_error" || e.RequestID != "req-1" {
		t.Fatalf("%+v", e)
	}
	if len(e.Fields) != 2 || e.Fields[0].Field != "k8s_version" {
		t.Fatalf("fields = %+v", e.Fields)
	}
}

func TestParse_PlainErrorEnvelope(t *testing.T) {
	e := Parse(http.StatusConflict, http.Header{}, []byte(`{"error":{"code":"conflict","message":"name already exists"}}`))
	if e.Code != "conflict" || e.Message != "name already exists" || len(e.Fields) != 0 {
		t.Fatalf("%+v", e)
	}
}

func TestParse_NonJSONBodyStillProducesAnError(t *testing.T) {
	e := Parse(http.StatusBadGateway, http.Header{}, []byte("<html>502</html>"))
	if e.Status != 502 || e.Message == "" {
		t.Fatalf("%+v", e)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(Parse(404, http.Header{}, []byte(`{"error":{"code":"not_found","message":"gone"}}`))) {
		t.Fatal("404 must be IsNotFound")
	}
	if IsNotFound(Parse(500, nil, nil)) { // a nil Header must be safe
		t.Fatal("500 must not be IsNotFound")
	}
}

func TestToDiagnostics_ValidationBecomesAttributeErrors(t *testing.T) {
	e := Parse(422, http.Header{}, []byte(`{"error":{"code":"validation_error","message":"invalid","details":[{"field":"k8s_version","code":"not_offered","message":"1.31 is not offered"}]}}`))
	var d diag.Diagnostics
	ToDiagnostics(e, &d, path.Empty())
	if len(d) != 1 {
		t.Fatalf("diags = %v", d)
	}
	ad, ok := d[0].(diag.DiagnosticWithPath)
	if !ok || !ad.Path().Equal(path.Root("k8s_version")) {
		t.Fatalf("expected an attribute error at k8s_version, got %v", d[0])
	}
}

func TestToDiagnostics_AuthIsOneProviderHint(t *testing.T) {
	var d diag.Diagnostics
	ToDiagnostics(Parse(403, http.Header{}, []byte(`{"error":{"code":"forbidden","message":"nope"}}`)), &d, path.Empty())
	if len(d) != 1 || !containsStr(d[0].Detail(), "api_token") {
		t.Fatalf("diags = %v", d)
	}
}

func TestToDiagnostics_RequestIDIsSurfaced(t *testing.T) {
	var d diag.Diagnostics
	hdr := http.Header{}
	hdr.Set("X-Request-Id", "req-9")
	ToDiagnostics(Parse(500, hdr, []byte(`{"error":{"code":"internal","message":"boom"}}`)), &d, path.Empty())
	if !containsStr(d[0].Detail(), "req-9") {
		t.Fatalf("detail = %q", d[0].Detail())
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

`internal/provider/transport/retry_test.go`:

```go
package transport

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBearerHeaderIsAdded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Get("Authorization") }))
	defer srv.Close()
	c := &http.Client{Transport: New("aether_x", http.DefaultTransport)}
	if _, err := c.Get(srv.URL); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer aether_x" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestRetriesOn429HonouringRetryAfter(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := &http.Client{Transport: New("t", http.DefaultTransport)}
	resp, err := c.Get(srv.URL)
	if err != nil || resp.StatusCode != 200 || atomic.LoadInt32(&n) != 2 {
		t.Fatalf("resp=%v err=%v n=%d", resp, err, n)
	}
}

func TestRetries5xxOnGetButNotOnPost(t *testing.T) {
	var gets, posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&posts, 1)
		} else {
			atomic.AddInt32(&gets, 1)
		}
		w.WriteHeader(503)
	}))
	defer srv.Close()
	tr := New("t", http.DefaultTransport).(*retryTransport)
	tr.backoff = func(int) time.Duration { return 0 }
	c := &http.Client{Transport: tr}
	_, _ = c.Get(srv.URL)
	_, _ = c.Post(srv.URL, "application/json", nil)
	if atomic.LoadInt32(&gets) != 4 { // 1 + 3 retries
		t.Fatalf("gets = %d, want 4", gets)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Fatalf("posts = %d, want 1 (POST is not retried on 5xx)", posts)
	}
}
```

- [ ] **Step 2 [TEST-RUNNER]:** RED (packages do not build).

- [ ] **Step 3 [CODER]: Implement**

`internal/provider/apierr/apierr.go`:

```go
// Package apierr turns Aether API error bodies into Terraform diagnostics.
package apierr

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

type FieldError struct {
	Field, Code, Message string
}

type Error struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	Fields    []FieldError
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("aether api: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("aether api: %d: %s", e.Status, e.Message)
}

// envelope matches ErrorResponse/ErrorBody in aether-api's
// internal/api/handler/response.go (@ 8f03fbb), which is what the vendored
// api/openapi.yaml describes. Field errors are error.details; there is no
// top-level "errors" list and no request_id in the body.
type envelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details []struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"details"`
	} `json:"error"`
}

// Parse reads an API error response. header is the RESPONSE header, which is
// where the request id lives; a nil header is safe (http.Header(nil).Get
// returns ""). It never returns nil.
func Parse(status int, header http.Header, body []byte) *Error {
	// Get canonicalises the key, so the wire's X-Request-ID matches.
	e := &Error{Status: status, RequestID: header.Get("X-Request-Id")}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || (env.Error.Message == "" && len(env.Error.Details) == 0) {
		e.Message = strings.TrimSpace(http.StatusText(status) + " " + truncate(string(body), 200))
		return e
	}
	e.Code, e.Message = env.Error.Code, env.Error.Message
	for _, f := range env.Error.Details {
		e.Fields = append(e.Fields, FieldError{Field: f.Field, Code: f.Code, Message: f.Message})
	}
	return e
}

func IsNotFound(err error) bool {
	e, ok := err.(*Error)
	return ok && e.Status == http.StatusNotFound
}

// ToDiagnostics appends diagnostics for err. attrPrefix is prepended to field paths
// (path.Empty() for top-level attributes).
func ToDiagnostics(err error, diags *diag.Diagnostics, attrPrefix path.Path) {
	e, ok := err.(*Error)
	if !ok {
		diags.AddError("Aether API request failed", err.Error())
		return
	}
	rid := ""
	if e.RequestID != "" {
		rid = " (request id " + e.RequestID + ")"
	}
	switch {
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		diags.AddError("Aether API rejected the request",
			fmt.Sprintf("%d %s%s. Check api_token (created in the portal) and, if set, organization_id.", e.Status, e.Message, rid))
	case e.Status == http.StatusUnprocessableEntity && len(e.Fields) > 0:
		for _, f := range e.Fields {
			p := attrPrefix
			for _, seg := range strings.Split(f.Field, ".") {
				if seg != "" {
					p = p.AtName(seg)
				}
			}
			diags.AddAttributeError(p, "Invalid value", fmt.Sprintf("%s (%s)%s", f.Message, f.Code, rid))
		}
	case e.Status == http.StatusConflict:
		diags.AddError("Conflict", e.Message+rid)
	default:
		diags.AddError("Aether API error", fmt.Sprintf("%d %s: %s%s", e.Status, e.Code, e.Message, rid))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
```

`internal/provider/transport/retry.go` — the rules, not a second copy of the code. A
reference implementation carried here diverged from the measured contract once already; the
implementation that satisfies every rule below is `internal/provider/transport/retry.go`,
and the tests above are the contract:

- Concrete type `retryTransport{token, next, backoff func(attempt int) time.Duration}`;
  `New` returns it as an `http.RoundTripper`. `backoff` is a FIELD, not a package function,
  because the tests type-assert and zero it.
- `RoundTrip` works on `req.Clone(req.Context())` — a RoundTripper must not mutate the
  caller's request — sets `Authorization: Bearer <token>`, and sets `User-Agent` only when
  the caller left it empty.
- **`maxRetries = 3`**, so an idempotent request makes at most **1 + 3 = 4** attempts.
  `defaultBackoff` is 500 ms → 1 s → 2 s, capped at 4 s, shift-guarded for large attempts.
- **429 is retried for every method.** `Retry-After` is read as **integer seconds only**
  (the HTTP-date form is legal but this API does not send it), negatives clamped to 0 and
  the value **clamped to ≤ 60 s** so a hostile or buggy header cannot park an apply; absent
  or unparseable, the backoff applies.
- **5xx is retried only for idempotent methods** — GET, HEAD, PUT, DELETE, OPTIONS. POST and
  PATCH surface a 5xx immediately: the server may have acted, and a replay bills the
  customer twice.
- A **transport error** is retried for idempotent methods. For POST/PATCH it is replayed
  **only when the failure proves nothing was written**: `errors.As` a `*net.DNSError`, or a
  `*net.OpError` with `Op == "dial"`. net/http exposes no "nothing written" signal of its
  own, so a connection that broke **mid-body is deliberately not replayed**.
- **Nothing is replayed unless the body can be re-opened**: `req.Body == nil`,
  `req.Body == http.NoBody`, or `req.GetBody != nil`. Each retry re-opens the body via
  `req.GetBody()`.
- Every **discarded** response is drained **bounded — `io.CopyN(io.Discard, resp.Body, 64<<10)`** —
  and closed, so the connection is reusable without reading an unbounded error page. The
  **final** response is returned **unread and unclosed**, so it propagates exactly as if
  there were no retry layer; in particular the 4th 5xx must still have a readable body.
- `sleep` waits on `req.Context().Done()` as well as the timer and returns the context
  error, so a cancelled apply does not sit out the remaining backoff.


- [ ] **Step 4 [TEST-RUNNER]:** `go test ./internal/provider/apierr/ ./internal/provider/transport/ -count=1 -race -v` → PASS.
- [ ] **Step 5 [CODER]: Commit** — `feat: API error mapping and retrying bearer transport`.

---

### Task 4: Wait helper

**Files:** Create `internal/provider/wait/wait.go`; Test `internal/provider/wait/wait_test.go`.

**Interfaces:** Produces
```go
type Probe func(ctx context.Context) (status, message string, err error)
type Options struct { Timeout time.Duration; Success, Warning, Failed []string; Now func() time.Time; Sleep func(context.Context, time.Duration) error }
type Result struct { Status, Message string; Warned bool }
func ForStatus(ctx context.Context, probe Probe, opts Options) (Result, error)
func ForGone(ctx context.Context, probe func(ctx context.Context) (gone bool, err error), opts Options) error
var ErrTimeout = errors.New("wait: timeout")
type FailedError struct { Status, Message string }   // implements error
```
`Now`/`Sleep` default to real time; tests inject fakes. Four rules are the contract:

- **Match the status on EVERY probe, including the first — check before sleep.** A
  sleep-then-check loop makes the fastest operation take a whole backoff interval and hides
  a resource that was already terminal when the wait began.
- **A `Warning` status is TERMINAL.** It ends the wait with `Result.Warned = true` and the
  API's message; the caller surfaces that as a **warning diagnostic** (a `degraded` cluster
  has finished coming up). Polling on would hang to the timeout and then report a failure
  that did not happen.
- **Backoff 1 s, 2 s, 4 s, 8 s, then 15 s — capped AFTER the doubling**, so the sequence
  never jumps 8 s → 16 s. Exactly one sleep per non-terminal probe.
- **A timeout wraps `ErrTimeout` with `%w` and NAMES the last observed status**; a `Failed`
  status returns `*FailedError` (a POINTER, so `errors.As` matches); a probe error
  short-circuits, wrapped so `errors.Is`/`errors.As` still reach the cause. The production
  `Sleep` honours context cancellation.

- [ ] **Step 1 [TEST-WRITER]:**

```go
package wait

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error { f.now = f.now.Add(d); return nil }

func opts(fc *fakeClock, timeout time.Duration) Options {
	return Options{Timeout: timeout, Success: []string{"running"}, Warning: []string{"degraded"}, Failed: []string{"failed"}, Now: fc.Now, Sleep: fc.Sleep}
}

func TestForStatus_ReachesSuccess(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	seq := []string{"provisioning", "provisioning", "running"}
	i := 0
	res, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { s := seq[i]; i++; return s, "", nil }, opts(fc, time.Hour))
	if err != nil || res.Status != "running" || res.Warned {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestForStatus_WarningIsTerminalAndFlagged(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	res, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "degraded", "one node NotReady", nil }, opts(fc, time.Hour))
	if err != nil || !res.Warned || res.Message != "one node NotReady" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestForStatus_FailedCarriesMessage(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	_, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "failed", "quota exceeded", nil }, opts(fc, time.Hour))
	var fe *FailedError
	if !errors.As(err, &fe) || fe.Message != "quota exceeded" {
		t.Fatalf("err = %v", err)
	}
}

func TestForStatus_TimeoutNamesLastStatus(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	_, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "provisioning", "", nil }, opts(fc, 2*time.Minute))
	if !errors.Is(err, ErrTimeout) || !contains(err.Error(), "provisioning") {
		t.Fatalf("err = %v", err)
	}
}

func TestForStatus_BackoffCapsAt15s(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	var sleeps []time.Duration
	o := opts(fc, time.Hour)
	o.Sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); fc.now = fc.now.Add(d); return nil }
	n := 0
	_, _ = ForStatus(context.Background(), func(context.Context) (string, string, error) {
		n++
		if n > 8 {
			return "running", "", nil
		}
		return "provisioning", "", nil
	}, o)
	if sleeps[0] != time.Second || sleeps[len(sleeps)-1] != 15*time.Second {
		t.Fatalf("sleeps = %v", sleeps)
	}
}

func TestForStatus_ProbeErrorIsReturned(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	boom := errors.New("boom")
	_, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "", "", boom }, opts(fc, time.Hour))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestForGone(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	n := 0
	err := ForGone(context.Background(), func(context.Context) (bool, error) { n++; return n >= 3, nil }, opts(fc, time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2 [TEST-RUNNER]:** RED.
- [ ] **Step 3 [CODER]:**

```go
// Package wait polls a resource until it reaches a terminal state.
package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrTimeout = errors.New("wait: timeout")

type FailedError struct{ Status, Message string }

func (e *FailedError) Error() string { return fmt.Sprintf("reached %s: %s", e.Status, e.Message) }

type Probe func(ctx context.Context) (status, message string, err error)

type Options struct {
	Timeout                   time.Duration
	Success, Warning, Failed  []string
	Now                       func() time.Time
	Sleep                     func(context.Context, time.Duration) error
}

type Result struct {
	Status, Message string
	Warned          bool
}

func (o *Options) defaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func in(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

func backoff(attempt int) time.Duration {
	d := time.Second << attempt
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}

func ForStatus(ctx context.Context, probe Probe, opts Options) (Result, error) {
	opts.defaults()
	deadline := opts.Now().Add(opts.Timeout)
	last := ""
	for attempt := 0; ; attempt++ {
		status, msg, err := probe(ctx)
		if err != nil {
			return Result{}, err
		}
		last = status
		switch {
		case in(opts.Success, status):
			return Result{Status: status, Message: msg}, nil
		case in(opts.Warning, status):
			return Result{Status: status, Message: msg, Warned: true}, nil
		case in(opts.Failed, status):
			return Result{}, &FailedError{Status: status, Message: msg}
		}
		if !opts.Now().Before(deadline) {
			return Result{}, fmt.Errorf("%w after %s; last status %q", ErrTimeout, opts.Timeout, last)
		}
		if err := opts.Sleep(ctx, backoff(attempt)); err != nil {
			return Result{}, err
		}
	}
}

func ForGone(ctx context.Context, probe func(ctx context.Context) (bool, error), opts Options) error {
	opts.defaults()
	deadline := opts.Now().Add(opts.Timeout)
	for attempt := 0; ; attempt++ {
		gone, err := probe(ctx)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}
		if !opts.Now().Before(deadline) {
			return fmt.Errorf("%w after %s; resource still present", ErrTimeout, opts.Timeout)
		}
		if err := opts.Sleep(ctx, backoff(attempt)); err != nil {
			return err
		}
	}
}
```

- [ ] **Step 4 [TEST-RUNNER]:** PASS `-race`. **Step 5 [CODER]:** commit `feat: wait helper (poll to terminal state, fake-clock testable)`.

---

### Task 5: `k8s_version` custom type with semantic equality + `IsForward`

**Files:** Create `internal/provider/versions/versions.go`; Test `internal/provider/versions/versions_test.go`.

**Interfaces:** Produces `versions.K8sVersionType{}` (a `basetypes.StringTypable`) and `versions.K8sVersionValue` (a `basetypes.StringValuableWithSemanticEquals`); `versions.NewK8sVersionValue(s string) K8sVersionValue`; `versions.SemanticallyEqual(configured, actual string) bool`; `versions.IsForward(from, to string) (bool, error)`; `versions.Minor(v string) (major, minor int, err error)`. No `latest` keyword at this layer.

**Direction is load-bearing.** The framework calls `proposedNew.StringSemanticEquals(ctx, prior)` and, when it returns true, keeps the **prior** value in state — measured in terraform-plugin-framework v1.19.0, `internal/fwschemadata/value_semantic_equality_string.go:37` (the call) and `:53` (`resp.NewValue = priorValuable`). So the **receiver is the API's value** and the **argument is the configured/prior** one, and the hook must compute `SemanticallyEqual(configured = argument, actual = receiver)`. The two orders are not the same function: `SemanticallyEqual("1.37", "1.37.0")` is true while `SemanticallyEqual("1.37.0", "1.37")` is false, so a hook written backwards is a real bug that a test whose operands happen to be ordered the same way will not catch.

- [ ] **Step 1 [TEST-WRITER]:**

```go
package versions

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

func TestSemanticallyEqual(t *testing.T) {
	cases := []struct {
		conf, actual string
		want         bool
	}{
		{"1.37", "1.37.0", true}, {"1.37", "1.37.4", true}, {"1.37.0", "1.37.0", true},
		{"1.37.0", "1.37.1", false}, {"1.37", "1.36.9", false}, {"v1.37", "1.37.0", true},
		{"1.37", "", false}, {"", "", true}, {"bogus", "1.37.0", false},
	}
	for _, c := range cases {
		if got := SemanticallyEqual(c.conf, c.actual); got != c.want {
			t.Errorf("SemanticallyEqual(%q,%q) = %v, want %v", c.conf, c.actual, got, c.want)
		}
	}
}

func TestIsForward(t *testing.T) {
	for _, c := range []struct {
		from, to string
		want     bool
		wantErr  bool
	}{{"1.36", "1.37", true, false}, {"1.36.4", "1.37", true, false}, {"1.37", "1.36", false, false}, {"1.37.0", "1.37.1", true, false}, {"1.37", "1.37", false, false}, {"x", "1.37", false, true}} {
		got, err := IsForward(c.from, c.to)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("IsForward(%q,%q) = %v,%v", c.from, c.to, got, err)
		}
	}
}

func TestValue_SemanticEqualsHook(t *testing.T) {
	// The framework calls proposedNew.StringSemanticEquals(ctx, prior)
	// (terraform-plugin-framework v1.19.0,
	// internal/fwschemadata/value_semantic_equality_string.go:37), so the
	// RECEIVER is the API's value and the ARGUMENT is the configured one.
	apiValue := NewK8sVersionValue("1.37.4")
	eq, diags := apiValue.StringSemanticEquals(context.Background(), NewK8sVersionValue("1.37"))
	if diags.HasError() || !eq {
		t.Fatalf("eq=%v diags=%v; a configured 1.37 must accept the API's 1.37.4", eq, diags)
	}
	// The SAME two strings in the other role must NOT be equal: a configured
	// 1.37.4 pins the patch, so an API value of 1.37 does not satisfy it. This
	// is the assertion that fails when the hook swaps its operands.
	reversed := NewK8sVersionValue("1.37")
	if eq, _ := reversed.StringSemanticEquals(context.Background(), NewK8sVersionValue("1.37.4")); eq {
		t.Fatal("configured 1.37.4 vs API 1.37 must not be equal — the hook has its operands reversed")
	}
	if eq, _ := apiValue.StringSemanticEquals(context.Background(), basetypes.NewStringValue("1.38")); eq {
		t.Fatal("configured 1.38 vs API 1.37.4 must not be equal")
	}
}
```

- [ ] **Step 2 [TEST-RUNNER]:** RED.
- [ ] **Step 3 [CODER]:**

```go
// Package versions holds the k8s_version custom type: "1.37" and "1.37.0" are the
// same configured intent, so the resolved patch the API reports is never drift.
package versions

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

var re = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)(?:\.(0|[1-9]\d*))?$`)

type parsed struct {
	major, minor, patch int
	hasPatch            bool
}

func parse(s string) (parsed, error) {
	m := re.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return parsed{}, fmt.Errorf("%q is not MAJOR.MINOR or MAJOR.MINOR.PATCH", s)
	}
	p := parsed{}
	p.major, _ = strconv.Atoi(m[1])
	p.minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		p.patch, _ = strconv.Atoi(m[3])
		p.hasPatch = true
	}
	return p, nil
}

func Minor(v string) (int, int, error) {
	p, err := parse(v)
	return p.major, p.minor, err
}

func SemanticallyEqual(configured, actual string) bool {
	if configured == "" && actual == "" {
		return true
	}
	c, err1 := parse(configured)
	a, err2 := parse(actual)
	if err1 != nil || err2 != nil {
		return false
	}
	if c.major != a.major || c.minor != a.minor {
		return false
	}
	if !c.hasPatch {
		return true
	}
	return a.hasPatch && c.patch == a.patch
}

// IsForward reports whether to is a newer version than from (minor, then patch).
func IsForward(from, to string) (bool, error) {
	f, err := parse(from)
	if err != nil {
		return false, err
	}
	t, err := parse(to)
	if err != nil {
		return false, err
	}
	if t.major != f.major {
		return t.major > f.major, nil
	}
	if t.minor != f.minor {
		return t.minor > f.minor, nil
	}
	return t.hasPatch && f.hasPatch && t.patch > f.patch, nil
}

// ---- custom type plumbing --------------------------------------------------

var _ basetypes.StringTypable = K8sVersionType{}

type K8sVersionType struct{ basetypes.StringType }

func (K8sVersionType) Equal(o attr.Type) bool { _, ok := o.(K8sVersionType); return ok }
func (K8sVersionType) String() string          { return "K8sVersionType" }
func (t K8sVersionType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return K8sVersionValue{StringValue: in}, nil
}
func (t K8sVersionType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	v, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}
	sv, ok := v.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected %T", v)
	}
	return K8sVersionValue{StringValue: sv}, nil
}
func (K8sVersionType) ValueType(context.Context) attr.Value { return K8sVersionValue{} }

var _ basetypes.StringValuableWithSemanticEquals = K8sVersionValue{}

type K8sVersionValue struct{ basetypes.StringValue }

func NewK8sVersionValue(s string) K8sVersionValue {
	return K8sVersionValue{StringValue: basetypes.NewStringValue(s)}
}
func (v K8sVersionValue) Type(context.Context) attr.Type { return K8sVersionType{} }
func (v K8sVersionValue) Equal(o attr.Value) bool {
	ov, ok := o.(K8sVersionValue)
	return ok && v.StringValue.Equal(ov.StringValue)
}
func (v K8sVersionValue) StringSemanticEquals(ctx context.Context, newValuable basetypes.StringValuable) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	nv, d := newValuable.ToStringValue(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}
	if v.IsNull() || v.IsUnknown() || nv.IsNull() || nv.IsUnknown() {
		return v.StringValue.Equal(nv), diags
	}
	// v is the RECEIVER = proposedNew = the API's value; nv is the ARGUMENT =
	// prior = what was configured. Do not swap these (see Interfaces above).
	return SemanticallyEqual(nv.ValueString(), v.ValueString()), diags
}
```

- [ ] **Step 4 [TEST-RUNNER]:** PASS. **Step 5 [CODER]:** commit `feat: k8s_version custom type with semantic equality`.

---

### Task 6: `aether_cluster` resource

**Files:** Create `internal/provider/resource_cluster.go`, `internal/provider/timeouts.go` (shared timeouts helpers); Modify `internal/provider/provider.go` (`Resources()` adds `NewClusterResource`); Test `internal/provider/resource_cluster_test.go` (+ fakeapi cluster routes added by the test-writer in `fakeapi/clusters.go`).

**Interfaces:**
- Consumes: `providerData`, `client`, `apierr`, `wait`, `versions`.
- Produces: `NewClusterResource() resource.Resource`; model `clusterModel` with `tfsdk` tags = attribute names below; helpers `defaultTimeouts(ctx, m timeouts.Value) (create, update, delete time.Duration)`.

Schema (attribute → framework type; C=Configured, P=Computed, R=RequiresReplace):
`id` P (UseStateForUnknown) · `name` C · `region` C+R · `k8s_version` C, `CustomType: versions.K8sVersionType{}` · `public_endpoint` C, Optional+Computed, `Default: booldefault.StaticBool(false)` · `plan` C Optional · `status`, `status_message`, `endpoint`, `observed_kubernetes_version`, `capi_cluster_name`, `keycloak_realm`, `harbor_project`, `ceph_pool`, `lb_subnet`, `worker_subnet`, `created_at`, `updated_at` P · `timeouts` (framework-timeouts `timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true, Delete: true})`).

- [ ] **Step 1 [TEST-WRITER]: fakeapi cluster routes + resource tests**

Add to `fakeapi` (new file `fakeapi/clusters.go`, package `fakeapi`): an in-memory map of clusters keyed by id, routes `GET/POST /organizations/{org}/clusters`, `GET/PUT/DELETE /organizations/{org}/clusters/{id}`, `POST …/upgrade`, with a `StatusSequence(id string, statuses ...string)` knob that makes successive GETs walk through the given statuses (last one repeats) and `DeleteBecomes404After(id string, n int)`. Cluster JSON = the `Cluster` schema (all fields; `status` from the sequence; `k8s_version` echoes the request; `observed_kubernetes_version` = request minor + ".0"). Wire it into `serve` via a route table (extend the `switch`).

`resource_cluster_test.go` drives the resource through the framework with `resource.UnitTest`-style tests using `terraform-plugin-testing` with `ProtoV6ProviderFactories` pointing at `New("test")` and `AETHER_ENDPOINT`/`AETHER_API_TOKEN` set to the fake (this IS the unit layer — the harness runs a real `terraform` binary; mark with `IsUnitTest: true` so `TF_ACC` is not required). Tests:

```go
package provider_test

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/plane-shift/terraform-provider-aether/internal/provider"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

func factories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"aether": providerserver.NewProtocol6WithError(provider.New("test")()),
	}
}

func setupFake(t *testing.T) *fakeapi.Server {
	t.Helper()
	api := fakeapi.New(t)
	api.AddOrg("11111111-1111-4111-8111-111111111111", "Acme", "acme")
	t.Setenv("AETHER_ENDPOINT", api.URL())
	t.Setenv("AETHER_API_TOKEN", api.Token())
	return api
}

const clusterCfg = `
provider "aether" {}
resource "aether_cluster" "c" {
  name        = "prod"
  region      = "eu-fi"
  k8s_version = "%s"
}
`

func TestClusterResource_CreateWaitsForRunningAndTolerates137vs1370(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "provisioning", "provisioning", "running")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(clusterCfg, "1.37"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("aether_cluster.c", "status", "running"),
				resource.TestCheckResourceAttr("aether_cluster.c", "k8s_version", "1.37"),
				resource.TestCheckResourceAttr("aether_cluster.c", "observed_kubernetes_version", "1.37.0"),
				resource.TestCheckResourceAttr("aether_cluster.c", "public_endpoint", "false"),
				resource.TestCheckResourceAttrSet("aether_cluster.c", "id"),
			),
		}, {
			// second plan must be empty: 1.37 vs API's 1.37.0 is not drift
			Config: fmt.Sprintf(clusterCfg, "1.37"), PlanOnly: true, ExpectNonEmptyPlan: false,
		}},
	})
}

func TestClusterResource_UpgradeGoesThroughUpgradeEndpoint(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: fmt.Sprintf(clusterCfg, "1.36")},
			{Config: fmt.Sprintf(clusterCfg, "1.37"), Check: func(s *terraformState) error {
				for _, r := range api.Requests() {
					if r.Method == http.MethodPost && regexp.MustCompile(`/clusters/[^/]+/upgrade$`).MatchString(r.Path) {
						return nil
					}
				}
				return fmt.Errorf("no POST …/upgrade was made")
			}},
		},
	})
}

func TestClusterResource_DowngradeIsRefusedAtPlan(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: fmt.Sprintf(clusterCfg, "1.37")},
			{Config: fmt.Sprintf(clusterCfg, "1.36"), ExpectError: regexp.MustCompile(`(?i)downgrade|backwards`)},
		},
	})
}

func TestClusterResource_FailedStatusSurfacesMessage(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "provisioning", "failed")
	api.SetStatusMessage("*", "no capacity in eu-fi")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{Config: fmt.Sprintf(clusterCfg, "1.37"), ExpectError: regexp.MustCompile(`no capacity in eu-fi`)}},
	})
}

func TestClusterResource_DeleteWaitsFor404AndRegionForcesReplace(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	api.DeleteBecomes404After("*", 2)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: fmt.Sprintf(clusterCfg, "1.37")},
			{Config: `provider "aether" {}
resource "aether_cluster" "c" { name = "prod" region = "eu-fi2" k8s_version = "1.37" }`,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction("aether_cluster.c", plancheck.ResourceActionReplace)}}},
		},
	})
}

func TestClusterResource_ImportByID(t *testing.T) {
	api := setupFake(t)
	api.StatusSequence("*", "running")
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: fmt.Sprintf(clusterCfg, "1.37")},
			{ResourceName: "aether_cluster.c", ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"timeouts"}},
		},
	})
}
```

(The test-writer fixes imports — `plancheck` from `github.com/hashicorp/terraform-plugin-testing/plancheck`, `terraform.State` for the check func — and the fake's `SetStatusMessage`. `"*"` means "every cluster".) The harness needs a `terraform` (or `tofu`) binary on PATH; the Makefile/CI installs one (Task 16). Locally: `brew install terraform` or `opentofu`.

- [ ] **Step 2 [TEST-RUNNER]:** RED (`aether_cluster` unknown resource type).

- [ ] **Step 3 [CODER]: Implement `resource_cluster.go`**

```go
package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/versions"
	"github.com/plane-shift/terraform-provider-aether/internal/provider/wait"
)

var (
	_ resource.Resource                = &clusterResource{}
	_ resource.ResourceWithConfigure   = &clusterResource{}
	_ resource.ResourceWithImportState = &clusterResource{}
	_ resource.ResourceWithModifyPlan  = &clusterResource{}
)

func NewClusterResource() resource.Resource { return &clusterResource{} }

type clusterResource struct{ pd *providerData }

type clusterModel struct {
	ID                        types.String              `tfsdk:"id"`
	Name                      types.String              `tfsdk:"name"`
	Region                    types.String              `tfsdk:"region"`
	K8sVersion                versions.K8sVersionValue  `tfsdk:"k8s_version"`
	PublicEndpoint            types.Bool                `tfsdk:"public_endpoint"`
	Plan                      types.String              `tfsdk:"plan"`
	Status                    types.String              `tfsdk:"status"`
	StatusMessage             types.String              `tfsdk:"status_message"`
	Endpoint                  types.String              `tfsdk:"endpoint"`
	ObservedKubernetesVersion types.String              `tfsdk:"observed_kubernetes_version"`
	CapiClusterName           types.String              `tfsdk:"capi_cluster_name"`
	KeycloakRealm             types.String              `tfsdk:"keycloak_realm"`
	HarborProject             types.String              `tfsdk:"harbor_project"`
	CephPool                  types.String              `tfsdk:"ceph_pool"`
	LbSubnet                  types.String              `tfsdk:"lb_subnet"`
	WorkerSubnet              types.String              `tfsdk:"worker_subnet"`
	CreatedAt                 types.String              `tfsdk:"created_at"`
	UpdatedAt                 types.String              `tfsdk:"updated_at"`
	Timeouts                  timeouts.Value            `tfsdk:"timeouts"`
}

func (r *clusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

func computedString() schema.StringAttribute {
	return schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}}
}

func (r *clusterResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed Kubernetes cluster (control plane). Node pools are separate `aether_node_pool` resources. Creation waits until the cluster is `running`; a `k8s_version` change performs an in-place upgrade through the upgrade endpoint and must move forward.",
		Attributes: map[string]schema.Attribute{
			"id":   schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name": schema.StringAttribute{Required: true},
			"region": schema.StringAttribute{Required: true, MarkdownDescription: "Region id from `data.aether_regions` (e.g. `eu-fi`). Changing it replaces the cluster.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"k8s_version": schema.StringAttribute{Required: true, CustomType: versions.K8sVersionType{},
				MarkdownDescription: "`MAJOR.MINOR` (resolves to the latest offered patch) or an explicit `MAJOR.MINOR.PATCH` of an offered minor. `1.37` and `1.37.0` are treated as equal. Only forward changes are allowed and go through an upgrade."},
			"public_endpoint": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Expose the API server on a public IP + DNS name (billed per public IP)."},
			"plan":                        schema.StringAttribute{Optional: true, MarkdownDescription: "Plan slug; omit for the default."},
			"status":                      computedString(),
			"status_message":              computedString(),
			"endpoint":                    computedString(),
			"observed_kubernetes_version": computedString(),
			"capi_cluster_name":           computedString(),
			"keycloak_realm":              computedString(),
			"harbor_project":              computedString(),
			"ceph_pool":                   computedString(),
			"lb_subnet":                   computedString(),
			"worker_subnet":               computedString(),
			"created_at":                  computedString(),
			"updated_at":                  computedString(),
			"timeouts":                    timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true, Delete: true}),
		},
	}
}

func (r *clusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan refuses a backwards k8s_version change before anything is applied.
func (r *clusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var state, plan clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() || plan.K8sVersion.IsUnknown() {
		return
	}
	from, to := state.K8sVersion.ValueString(), plan.K8sVersion.ValueString()
	if versions.SemanticallyEqual(to, from) {
		return
	}
	fwd, err := versions.IsForward(from, to)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("k8s_version"), "Invalid k8s_version", err.Error())
		return
	}
	if !fwd {
		resp.Diagnostics.AddAttributeError(path.Root("k8s_version"), "Kubernetes downgrade is not supported",
			fmt.Sprintf("k8s_version would move backwards from %s to %s. Aether only upgrades clusters; recreate the cluster to run an older version.", from, to))
	}
}

func (r *clusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	createTimeout, d := plan.Timeouts.Create(ctx, 30*time.Minute)
	resp.Diagnostics.Append(d...)

	body := client.CreateClusterJSONRequestBody{
		Name: plan.Name.ValueString(), Region: plan.Region.ValueString(), K8sVersion: plan.K8sVersion.ValueString(),
	}
	if !plan.PublicEndpoint.IsNull() {
		v := plan.PublicEndpoint.ValueBool()
		body.PublicEndpoint = &v
	}
	if !plan.Plan.IsNull() {
		v := plan.Plan.ValueString()
		body.Plan = &v
	}
	res, err := r.pd.Client.CreateClusterWithResponse(ctx, r.pd.OrgID, body)
	if err != nil {
		resp.Diagnostics.AddError("Cannot create cluster", err.Error())
		return
	}
	if res.StatusCode() != http.StatusCreated && res.StatusCode() != http.StatusAccepted && res.StatusCode() != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	created := clusterFromResponse(res.Body)
	plan.ID = types.StringValue(created.Id)
	// Persist the id immediately so a wait failure leaves an importable resource in state.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)

	final, err := r.waitForCluster(ctx, created.Id, createTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not become running", err.Error())
		return
	}
	r.flatten(final.Cluster, &plan)
	if final.Warned {
		resp.Diagnostics.AddWarning("Cluster is degraded", final.Message)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *clusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.pd.Client.GetClusterWithResponse(ctx, r.pd.OrgID, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot read cluster", err.Error())
		return
	}
	if res.StatusCode() == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}
	if res.StatusCode() != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	r.flatten(res.JSON200, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *clusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state clusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	updateTimeout, d := plan.Timeouts.Update(ctx, 30*time.Minute)
	resp.Diagnostics.Append(d...)
	id := state.ID.ValueString()

	// 1. mutable scalar fields via PUT
	if plan.Name != state.Name || plan.PublicEndpoint != state.PublicEndpoint {
		body := client.UpdateClusterJSONRequestBody{}
		if plan.Name != state.Name {
			v := plan.Name.ValueString()
			body.Name = &v
		}
		if plan.PublicEndpoint != state.PublicEndpoint {
			v := plan.PublicEndpoint.ValueBool()
			body.PublicEndpoint = &v
		}
		res, err := r.pd.Client.UpdateClusterWithResponse(ctx, r.pd.OrgID, id, body)
		if err != nil {
			resp.Diagnostics.AddError("Cannot update cluster", err.Error())
			return
		}
		if res.StatusCode() != http.StatusOK {
			apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.Body), &resp.Diagnostics, path.Empty())
			return
		}
	}
	// 2. k8s_version via the upgrade endpoint (ModifyPlan already refused backwards moves)
	if !versions.SemanticallyEqual(plan.K8sVersion.ValueString(), state.K8sVersion.ValueString()) {
		res, err := r.pd.Client.UpgradeClusterWithResponse(ctx, r.pd.OrgID, id, client.UpgradeClusterJSONRequestBody{TargetVersion: plan.K8sVersion.ValueString()})
		if err != nil {
			resp.Diagnostics.AddError("Cannot upgrade cluster", err.Error())
			return
		}
		if res.StatusCode() >= 300 {
			apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.Body), &resp.Diagnostics, path.Root("k8s_version"))
			return
		}
	}
	final, err := r.waitForCluster(ctx, id, updateTimeout)
	if err != nil {
		resp.Diagnostics.AddError("Cluster did not return to running", err.Error())
		return
	}
	plan.ID = state.ID
	r.flatten(final.Cluster, &plan)
	if final.Warned {
		resp.Diagnostics.AddWarning("Cluster is degraded", final.Message)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *clusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state clusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteTimeout, d := state.Timeouts.Delete(ctx, 20*time.Minute)
	resp.Diagnostics.Append(d...)
	id := state.ID.ValueString()
	res, err := r.pd.Client.DeleteClusterWithResponse(ctx, r.pd.OrgID, id)
	if err != nil {
		resp.Diagnostics.AddError("Cannot delete cluster", err.Error())
		return
	}
	if res.StatusCode() != http.StatusNotFound && res.StatusCode() >= 300 {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	err = wait.ForGone(ctx, func(ctx context.Context) (bool, error) {
		g, err := r.pd.Client.GetClusterWithResponse(ctx, r.pd.OrgID, id)
		if err != nil {
			return false, err
		}
		return g.StatusCode() == http.StatusNotFound, nil
	}, wait.Options{Timeout: deleteTimeout})
	if err != nil {
		resp.Diagnostics.AddError("Cluster deletion did not complete", err.Error())
	}
}

func (r *clusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

type clusterWait struct {
	Cluster *client.Cluster
	Message string
	Warned  bool
}

func (r *clusterResource) waitForCluster(ctx context.Context, id string, timeout time.Duration) (clusterWait, error) {
	var last *client.Cluster
	res, err := wait.ForStatus(ctx, func(ctx context.Context) (string, string, error) {
		g, err := r.pd.Client.GetClusterWithResponse(ctx, r.pd.OrgID, id)
		if err != nil {
			return "", "", err
		}
		if g.StatusCode() != http.StatusOK {
			return "", "", apierr.Parse(g.StatusCode(), g.Body)
		}
		last = g.JSON200
		return string(last.Status), deref(last.StatusMessage), nil
	}, wait.Options{Timeout: timeout, Success: []string{"running"}, Warning: []string{"degraded"}, Failed: []string{"failed"}})
	if err != nil {
		return clusterWait{}, err
	}
	return clusterWait{Cluster: last, Message: res.Message, Warned: res.Warned}, nil
}

func (r *clusterResource) flatten(c *client.Cluster, m *clusterModel) {
	m.ID = types.StringValue(c.Id)
	m.Name = types.StringValue(c.Name)
	m.Region = types.StringValue(c.Region)
	// keep the configured spelling when it is semantically equal to what the API holds
	if !versions.SemanticallyEqual(m.K8sVersion.ValueString(), c.K8sVersion) || m.K8sVersion.IsNull() || m.K8sVersion.IsUnknown() {
		m.K8sVersion = versions.NewK8sVersionValue(c.K8sVersion)
	}
	m.PublicEndpoint = types.BoolValue(c.PublicEndpoint)
	m.Plan = optString(c.PlanSlug)
	m.Status = types.StringValue(string(c.Status))
	m.StatusMessage = optString(c.StatusMessage)
	m.Endpoint = optString(c.Endpoint)
	m.ObservedKubernetesVersion = optString(c.ObservedKubernetesVersion)
	m.CapiClusterName = optString(c.CapiClusterName)
	m.KeycloakRealm = optString(c.KeycloakRealm)
	m.HarborProject = optString(c.HarborProject)
	m.CephPool = optString(c.CephPool)
	m.LbSubnet = optString(c.LbSubnet)
	m.WorkerSubnet = optString(c.WorkerSubnet)
	m.CreatedAt = types.StringValue(c.CreatedAt.Format(time.RFC3339))
	m.UpdatedAt = types.StringValue(c.UpdatedAt.Format(time.RFC3339))
}

// helpers shared by every resource (put in internal/provider/helpers.go)
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func optString(p *string) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(*p)
}
func clusterFromResponse(body []byte) *client.Cluster { var c client.Cluster; _ = json.Unmarshal(body, &c); return &c }
```

Adjust to the generated types: if `CreatedAt` is `time.Time` use `.Format`, if `string` assign directly; `Status` may be `client.ClusterStatus` (string alias). `plan` when null in config but returned by the API: keep it `Optional+Computed` with `UseStateForUnknown` if the API always echoes a slug (verify against the fake; the test-writer's fake returns `plan_slug: null` unless set). Register in `provider.go`: `Resources()` returns `[]func() resource.Resource{NewClusterResource}`.

- [ ] **Step 4 [TEST-RUNNER]:** `go test ./internal/provider/ -run TestClusterResource -count=1 -v` → PASS (needs `terraform` or `tofu` on PATH; if only `tofu`, `export TF_ACC_TERRAFORM_PATH=$(which tofu) TF_ACC_PROVIDER_HOST=registry.opentofu.org`).
- [ ] **Step 5 [CODER]:** commit `feat: aether_cluster resource (create/wait, upgrade, delete-to-404, import)`.

---

### Task 7: `aether_node_pool` resource

**Files:** Create `internal/provider/resource_node_pool.go`; Modify `provider.go` (register `NewNodePoolResource`); Test `resource_node_pool_test.go` + `fakeapi/nodepools.go` (routes `…/clusters/{cid}/nodepools[/{id}]`, `StatusSequence` semantics as clusters with statuses `provisioning|ready|scaling|deleting|failed`; the fake applies API defaults on create: `desired_nodes`→1, `min_nodes`/`max_nodes`→`desired_nodes` when null; echoes labels/taints; `current_nodes` = desired once `ready`).

**Interfaces:** Produces `NewNodePoolResource()`; model attributes exactly:
`id` P · `cluster_id` C+R · `name` C · `vcpus`,`memory_gb`,`disk_gb` C (Int64, Required) · `desired_nodes`,`min_nodes`,`max_nodes` Optional+Computed Int64 (`UseStateForUnknown`) · `min_vcpus`,`max_vcpus`,`min_memory_gb`,`max_memory_gb` Optional Int64 (description: "setting a max above its min makes the pool autoscaling — billed hourly per node; fixed pools bill a flat monthly fee") · `node_labels` Optional Map[String] +R · `node_taints` Optional List of nested `{key, value, effect}` +R (effect validated `oneOf NoSchedule|PreferNoSchedule|NoExecute`) · `drain_timeout_seconds` Optional Int64 (null = platform default; NOT computed — stays null) · `scale_down_delay_seconds`, `scale_down_utilization_percent` Optional+Computed Int64 · `current_nodes` P Int64 · `status`,`status_message`,`first_ready_at`,`created_at`,`updated_at` P · `timeouts` (create/update/delete).

- [ ] **Step 1 [TEST-WRITER]:** tests in the same style as Task 6 (`resource.UnitTest` over the fake):
  1. create a fixed pool `{vcpus=2, memory_gb=4, disk_gb=20}` → waits `provisioning→ready`; `desired_nodes`=1, `min_nodes`=1, `max_nodes`=1 reflected; second plan empty.
  2. autoscaling pool `{min_vcpus=2,max_vcpus=8,min_memory_gb=4,max_memory_gb=16}` → PUT body on a later `max_vcpus=12` change contains only changed fields (assert on `api.Requests()`), waits to `ready`.
  3. changing `node_labels` plans a replace (`plancheck.ExpectResourceAction(…, ResourceActionReplace)`); changing `vcpus` plans an update.
  4. `drain_timeout_seconds` omitted → request body has no `drain_timeout_seconds` key (assert bytes) and state stays null.
  5. delete waits for 404; import by id (`ImportStateVerifyIgnore: timeouts`).
  6. `effect = "Sometimes"` fails validation at plan.

- [ ] **Step 2 [TEST-RUNNER]:** RED.
- [ ] **Step 3 [CODER]:** implement mirroring Task 6's structure: schema with `int64planmodifier.UseStateForUnknown()` on the Optional+Computed ints, `mapplanmodifier.RequiresReplace()` / `listplanmodifier.RequiresReplace()` on labels/taints, `stringvalidator.OneOf("NoSchedule","PreferNoSchedule","NoExecute")` on `effect` (`github.com/hashicorp/terraform-plugin-framework-validators` — add `go get …-validators@latest` and pin), Create → `CreateNodePoolWithResponse(ctx, org, clusterID, body)` building `*int64` only for non-null attributes, wait `Success: ready`, `Failed: failed`; Update → PUT with only changed scalar fields then wait; Delete → DELETE then `ForGone`; `flatten` maps every field (`node_labels` → `types.MapValueFrom`, taints → nested list via `types.ListValueFrom` with an object type `{key,value,effect}`); ImportState passthrough on `id` — Read needs `cluster_id` too, so import id format is `<cluster_id>/<pool_id>` (document it; `ImportState` splits on `/` and sets both).
- [ ] **Step 4 [TEST-RUNNER]:** PASS. **Step 5:** commit `feat: aether_node_pool resource`.

---

### Task 8: `aether_cluster_network` resource

**Files:** Create `internal/provider/resource_cluster_network.go`; register; Test `resource_cluster_network_test.go` + fake routes `GET/PUT …/clusters/{cid}/network` (fake stores the last PUT; GET before any PUT returns `{"cluster_id":…, "authorized_cidrs":[], "api_server_rate_limit":{"max_connections_per_second":0,"burst":0}, "updated_at":…}`).

**Schema:** `cluster_id` Required+R · `authorized_cidrs` Required List[String] · `api_server_rate_limit` Optional+Computed SingleNested `{max_connections_per_second Int64, burst Int64}` (UseStateForUnknown) · `updated_at` P · `id` P (= cluster_id, for import ergonomics).

- [ ] **Step 1 [TEST-WRITER]:** tests: create PUTs the CIDRs; update PUTs the new list; delete PUTs `authorized_cidrs: []` (assert the last request body) and the description says so; import by cluster id.
- [ ] **Step 2:** RED. **Step 3 [CODER]:** Create/Update both call `UpsertClusterNetworkWithResponse`; Delete PUTs an empty list and the zero rate limit; Read GETs (404 → remove). **Step 4:** PASS. **Step 5:** commit `feat: aether_cluster_network resource`.

---

### Task 9: `aether_organization` and `aether_organization_member`

**Files:** Create `resource_organization.go`, `resource_organization_member.go`; register; tests + fake routes (`GET/PUT /organizations/{id}`; members CRUD under `/organizations/{id}/members[/{mid}]`).

**Organization schema:** `id` P · `name`, `slug`, `billing_email`, `vat_id` Optional+Computed String (UseStateForUnknown) · `plan`, `created_at`, `updated_at` P. Create is REFUSED: `resp.Diagnostics.AddError("Organizations are created at registration", "Import your organization instead: terraform import aether_organization.<name> <organization id> (data.aether_organization exposes the id).")`. Delete is STATE-ONLY: it makes no API call and returns success (the framework removes the resource from state); the schema description says "destroy only forgets the organization; it is never deleted". Update → PUT changed fields. Import passthrough.

**Member schema:** `id` P · `email` Required+R · `role` Required, `stringvalidator.OneOf("owner","admin","member","viewer")` · `invited_by`, `accepted_at`, `created_at` P.

- [ ] **Step 1 [TEST-WRITER]:** organization: `resource "aether_organization" "o" { name = "Acme" }` without import → `ExpectError` "created at registration"; import then update `name` → PUT; destroy succeeds and makes NO DELETE request (assert on `api.Requests()`). Member: create → POST body `{email, role}`; role change → PUT; email change → replace; delete → DELETE; import by id.
- [ ] **Step 2:** RED. **Step 3 [CODER]:** implement. **Step 4:** PASS. **Step 5:** commit `feat: aether_organization (import-only) and aether_organization_member`.

---

### Task 10: `aether_webhook`

**Files:** Create `resource_webhook.go`; register; tests + fake routes (CRUD + `POST …/webhooks/{id}/rotate-secret` returning the webhook with a new `secret`).

**Schema:** `id` P · `url` Required · `description` Optional+Computed · `events` Required Set[String] · `enabled` Optional+Computed Bool default true · `secret` P Sensitive (UseStateForUnknown) · `rotate_secret` Optional String (description: "change this value to rotate the secret; the value itself is not sent to the API") · `created_at`, `updated_at` P.

- [ ] **Step 1 [TEST-WRITER]:** create → POST `{url, description, events}`; `enabled=false` → PUT; changing `rotate_secret` from `"1"` to `"2"` → a `rotate-secret` POST and `secret` changes; import with `ImportStateVerifyIgnore: rotate_secret, secret` (secret is only returned on create/rotate? — verify in the fake per the schema: `Webhook.secret` IS in the read shape; keep it verifiable if the fake returns it).
- [ ] **Steps 2–5** as before; commit `feat: aether_webhook resource with secret rotation trigger`.

---

### Task 11: registry resources — `aether_registry`, `aether_registry_robot_account`, `aether_registry_cleanup_policy`

**Files:** Create the three resource files; register; tests + fake routes (`GET/POST/DELETE /organizations/{id}/registry` — POST returns `EnableRegistryResponse` with credentials; robot accounts `GET/POST …/robot-accounts`, `DELETE …/{id}`, POST returns `RobotAccountWithSecret`; cleanup policies CRUD).

**`aether_registry` schema:** `id` P (= registry id) · `harbor_project`, `endpoint`, `enabled`, `quota_gb`, `used_gb`, `created_at`, `updated_at` P · `robot_username`, `robot_secret` P Sensitive (from the enable response; `UseStateForUnknown`; description: write-once, empty after import). Create → POST (409 = already enabled → then GET and adopt); Read → GET (`RegistryDisabled{enabled:false}` → remove from state); Delete → DELETE with description warning that it removes the org's Harbor project. Import by organization id (any value → GET).

**Robot account:** `id` P · `name` Required+R · `permissions` Optional Set[String] +R · `secret` P Sensitive write-once · `harbor_id`, `last_used_at`, `created_at`, `updated_at` P. No Update (any change replaces).

**Cleanup policy:** `id` P · `name` Required · `action` Optional+Computed · `conditions` Required List nested `{type String, op String, value String}` (API `value` is untyped — the provider sends the string; the description says numbers are sent as strings) · `repo_scope` Optional+Computed Set[String] · `schedule` Optional+Computed · `enabled` Optional+Computed Bool · `last_run_at`, `last_run_deleted`, `last_run_freed_bytes`, `last_run_message` P.

- [ ] **Step 1 [TEST-WRITER]:** registry enable→state has credentials; second plan empty; delete → DELETE; robot create → POST returns secret, plan change of `permissions` → replace, delete; cleanup policy CRUD + import.
- [ ] **Steps 2–5**; commit `feat: registry, robot account and cleanup policy resources`.

---

### Task 12: `aether_billing_budget`

**Files:** Create `resource_billing_budget.go`; register; tests + fake (`GET/PUT/DELETE /organizations/{id}/billing/budget`).

**Schema:** `id` P (= org id) · `monthly_limit_eur` Required Float64 · `thresholds_pct` Required List[Int64] · `created_at`, `updated_at` P. Create/Update → PUT; Delete → DELETE; Read 404 → remove; import by org id.

- [ ] **Steps 1–5** (tests: create PUTs, update PUTs, delete DELETEs, import); commit `feat: aether_billing_budget resource`.

---

### Task 13: Cluster-scoped data sources

**Files:** Create `internal/provider/paging.go`, `datasource_organization.go`, `datasource_regions.go`, `datasource_k8s_versions.go`, `datasource_cluster.go`, `datasource_clusters.go`, `datasource_node_pool.go`, `datasource_node_pools.go`, `datasource_node_pool_machines.go`, `datasource_cluster_network.go`, `datasource_kubeconfig.go`, `datasource_loadbalancers.go`, `datasource_cluster_volumes.go`; register all in `DataSources()`; tests `datasource_*_test.go` + fake routes for regions, k8s-versions, kubeconfig (`{"kubeconfig": "apiVersion: v1…"}` — verify the exact key in the spec), loadbalancers, volumes, machines.

**`paging.go`:**

```go
package provider

import "context"

// collectAll drains a cursor-paginated list. fetch returns the page items and the next cursor ("" when done).
func collectAll[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) ([]T, string, error)) ([]T, error) {
	var out []T
	cursor := ""
	for {
		items, next, err := fetch(ctx, cursor)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" || next == cursor {
			return out, nil
		}
		cursor = next
	}
}
```

**Complete reference implementation — `datasource_regions.go`** (every other data source in this task follows this file exactly, substituting the table row):

```go
package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

var _ datasource.DataSourceWithConfigure = &regionsDataSource{}

func NewRegionsDataSource() datasource.DataSource { return &regionsDataSource{} }

type regionsDataSource struct{ pd *providerData }

type regionModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Location  types.String `tfsdk:"location"`
	Available types.Bool   `tfsdk:"available"`
}

type regionsModel struct {
	Regions []regionModel `tfsdk:"regions"`
}

func (d *regionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_regions"
}

func (d *regionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Regions the platform knows about. `available` is true only for the region this API instance serves.",
		Attributes: map[string]schema.Attribute{
			"regions": schema.ListNestedAttribute{Computed: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
				"id": schema.StringAttribute{Computed: true}, "name": schema.StringAttribute{Computed: true},
				"location": schema.StringAttribute{Computed: true}, "available": schema.BoolAttribute{Computed: true},
			}}},
		},
	}
}

func (d *regionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.pd = fromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *regionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	res, err := d.pd.Client.ListRegionsWithResponse(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot list regions", err.Error())
		return
	}
	if res.StatusCode() != http.StatusOK {
		apierr.ToDiagnostics(apierr.Parse(res.StatusCode(), res.Body), &resp.Diagnostics, path.Empty())
		return
	}
	var out regionsModel
	for _, r := range res.JSON200.Items {
		out.Regions = append(out.Regions, regionModel{ID: types.StringValue(r.Id), Name: types.StringValue(r.Name), Location: types.StringValue(r.Location), Available: types.BoolValue(r.Available)})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
```

| Data source | Client call | Inputs | Output attributes |
|---|---|---|---|
| `aether_organization` | `GetOrganizationWithResponse(org)` | — | `id`, `name`, `slug`, `billing_email`, `vat_id`, `plan`, `created_at`, `updated_at` |
| `aether_regions` | `ListRegionsWithResponse` | — | `regions[]{id,name,location,available}` |
| `aether_k8s_versions` | `ListK8sVersionsWithResponse` | — | `versions[]{version,label,patch}`, `latest` (the `Latest` item's `patch`) |
| `aether_cluster` | `GetClusterWithResponse` or `collectAll(ListClusters…)` filtered by `name` | exactly one of `id`, `name` (validator `ExactlyOneOf`) | all `aether_cluster` computed + configured fields |
| `aether_clusters` | `collectAll(ListClustersWithResponse)` | — | `clusters[]{…same fields…}` |
| `aether_node_pool` | `GetNodePoolWithResponse(org, cluster_id, id)` or list filtered by `name` | `cluster_id` + one of `id`/`name` | node pool fields |
| `aether_node_pools` | `collectAll(ListNodePools…)` | `cluster_id` | `node_pools[]` |
| `aether_node_pool_machines` | `ListNodePoolMachinesWithResponse` | `cluster_id`, `node_pool_id` | `machines[]{name,status,k8s_version,internal_ip,age_seconds,created_at}` |
| `aether_cluster_network` | `GetClusterNetworkWithResponse` | `cluster_id` | `authorized_cidrs`, `api_server_rate_limit{…}`, `updated_at` |
| `aether_kubeconfig` | `GetKubeconfigWithResponse` | `cluster_id` | `content` (Sensitive) — the exact JSON key comes from the `Kubeconfig`-shaped response in the spec; implementer reads it from `client.gen.go` |
| `aether_loadbalancers` | `ListLoadBalancersWithResponse` | `cluster_id` | `loadbalancers[]{service_name,namespace,external_ip,dns_name,ports}` |
| `aether_cluster_volumes` | `ListClusterVolumesWithResponse` | `cluster_id` | `volumes[]` with the `Volume`-shaped fields from the spec |

- [ ] **Step 1 [TEST-WRITER]:** one `resource.UnitTest` per data source over the fake: read returns the fake's items; `aether_cluster` by `name` with two matches → error "matched 2 clusters"; `aether_clusters` walks two pages (fake returns `next_cursor` on page 1); `aether_kubeconfig.content` is set and marked sensitive.
- [ ] **Step 2:** RED. **Step 3 [CODER]:** implement per the table using the reference file. **Step 4:** PASS. **Step 5:** commit `feat: cluster-scoped data sources`.

---

### Task 14: Organization / registry / billing data sources

**Files:** Create `datasource_organization_members.go`, `datasource_webhooks.go`, `datasource_registry.go`, `datasource_registry_repositories.go`, `datasource_registry_repository_tags.go`, `datasource_registry_robot_accounts.go`, `datasource_registry_cleanup_policies.go`, `datasource_audit_log.go`, `datasource_usage.go`, `datasource_cost_breakdown.go`, `datasource_invoices.go`; register; tests + fake routes.

| Data source | Client call | Inputs | Output |
|---|---|---|---|
| `aether_organization_members` | `collectAll(ListMembers…)` | — | `members[]{id,email,role,invited_by,accepted_at,created_at}` |
| `aether_webhooks` | `collectAll(ListWebhooks…)` | — | `webhooks[]{id,url,description,events,enabled,created_at,updated_at}` (no secret) |
| `aether_registry` | `GetRegistryWithResponse` | — | `enabled`, `harbor_project`, `endpoint`, `quota_gb`, `used_gb` |
| `aether_registry_repositories` | `collectAll(ListRepositories…)` | — | `repositories[]` (fields of `Repository`) |
| `aether_registry_repository_tags` | `collectAll(ListRepositoryTags…)` | `repository` | `tags[]` (fields of `Tag` incl. `VulnSummary` counts) |
| `aether_registry_robot_accounts` | `collectAll(ListRobotAccounts…)` | — | `robot_accounts[]{id,name,permissions,harbor_id,last_used_at,created_at}` |
| `aether_registry_cleanup_policies` | `collectAll(ListCleanupPolicies…)` | — | `policies[]` |
| `aether_audit_log` | `collectAll(ListAuditLog…)` with optional filters the spec exposes (`limit` capped) | optional filters | `events[]` (fields of `AuditEvent`) |
| `aether_usage` | `GetUsageWithResponse` | optional `month` if the spec has it | `total_eur`, `subscriptions[]`, `autoscaling[]` per `UsageSummary` |
| `aether_cost_breakdown` | `GetCostBreakdownWithResponse` | — | fields of `CostBreakdown` |
| `aether_invoices` | `collectAll(ListInvoices…)` | — | `invoices[]` (fields of `Invoice`) |

Same reference implementation as Task 13. A 404 from any data-source read is an ERROR ("not found"), never an empty result. The implementer reads the exact nested field names from `client.gen.go` for `Repository`, `Tag`, `VulnSummary`, `AuditEvent`, `UsageSummary`, `CostBreakdown`, `Invoice` and mirrors them 1:1 in snake_case.

- [ ] **Steps 1–5** as Task 13; commit `feat: organization, registry and billing data sources`.

---

### Task 15: Acceptance tests against dev (both engines)

**Files:** Create `internal/acctest/acctest.go`, `internal/acctest/cluster_lifecycle_test.go`, `internal/acctest/org_access_test.go`, `internal/acctest/registry_billing_test.go`; Modify `Makefile` (`testacc`).

**Prerequisites (owner):** a dedicated dev organization + API token created in the portal; exported as `AETHER_API_TOKEN`; `AETHER_ENDPOINT=https://dev.api.aetherplatform.cloud`; `terraform` AND `tofu` binaries installed.

`acctest.go`:

```go
package acctest

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/plane-shift/terraform-provider-aether/internal/provider"
)

var ProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"aether": providerserver.NewProtocol6WithError(provider.New("acc")()),
}

func PreCheck(t *testing.T) {
	t.Helper()
	for _, v := range []string{"AETHER_API_TOKEN", "AETHER_ENDPOINT"} {
		if os.Getenv(v) == "" {
			t.Fatalf("%s must be set for acceptance tests", v)
		}
	}
	if os.Getenv("AETHER_ENDPOINT") == "https://api.aetherplatform.cloud" {
		t.Fatal("refusing to run acceptance tests against PROD; point AETHER_ENDPOINT at dev")
	}
}
```

Scenarios (each a `resource.Test` with `PreCheck`, serial, `CheckDestroy` verifying 404):

1. **cluster_lifecycle**: `aether_cluster` (`k8s_version` = the OLDEST minor from `data.aether_k8s_versions` so an upgrade step exists) + one `aether_node_pool` `{vcpus=2, memory_gb=4, disk_gb=20}` → check `status=running`, pool `status=ready`, `current_nodes=1`; step 2 pool `vcpus=4`; step 3 cluster `k8s_version` = next minor → `observed_kubernetes_version` starts with it; step 4 `aether_cluster_network` with one CIDR; step 5 `data.aether_kubeconfig` content non-empty and a Go check that parses it (`clientcmd` is NOT a dependency — just assert it contains `server: https://`); destroy → `CheckDestroy` GETs return 404. Timeouts: 40m create/update.
2. **org_access**: import `aether_organization` by id from `data.aether_organization`, update `billing_email`; `aether_organization_member` (a `+acc` alias of the owner's email), `aether_webhook` with rotation.
3. **registry_billing**: `aether_registry` enable → robot account → cleanup policy → disable; `aether_billing_budget` set/change/delete.

Run: `TF_ACC=1 AETHER_ENDPOINT=https://dev.api.aetherplatform.cloud AETHER_API_TOKEN=… go test ./internal/acctest/ -count=1 -v -timeout 120m -p 1` and again with `TF_ACC_TERRAFORM_PATH=$(which tofu) TF_ACC_PROVIDER_HOST=registry.opentofu.org`.

- [ ] **Step 1 [TEST-WRITER]:** write the three files. **Step 2 [TEST-RUNNER]:** run against dev with both engines; paste results. **Step 3 [CODER]:** fix code defects the run exposes (never the tests). **Step 4:** commit `test: acceptance suite against dev (terraform + tofu)`.

---

### Task 16: Docs generation, CI, release

**Files:** Create `examples/provider/provider.tf`, `examples/resources/aether_cluster/resource.tf`, `examples/resources/aether_node_pool/resource.tf`, `examples/resources/aether_cluster_network/resource.tf`, `examples/resources/aether_organization/import.sh`, `examples/resources/aether_webhook/resource.tf`, `examples/resources/aether_registry/resource.tf`, `examples/data-sources/aether_kubeconfig/data-source.tf`, `templates/index.md.tmpl`, `docs/` (generated), `.goreleaser.yml`, `terraform-registry-manifest.json`, `.github/workflows/build-test.yml`, `.github/workflows/release.yml`, `.github/workflows/acceptance.yml`, `README.md`.

`examples/provider/provider.tf`:

```hcl
terraform {
  required_version = ">= 1.5" # OpenTofu >= 1.6 works the same way
  required_providers {
    aether = {
      source  = "plane-shift/aether"
      version = "~> 0.1"
    }
  }
}

# Create an API token in the Aether portal first (Organization → API tokens).
provider "aether" {
  api_token = var.aether_api_token # or AETHER_API_TOKEN
}
```

`examples/resources/aether_cluster/resource.tf`:

```hcl
data "aether_k8s_versions" "offered" {}

resource "aether_cluster" "prod" {
  name            = "prod"
  region          = "eu-fi"
  k8s_version     = data.aether_k8s_versions.offered.latest
  public_endpoint = true
}

resource "aether_node_pool" "workers" {
  cluster_id    = aether_cluster.prod.id
  name          = "workers"
  vcpus         = 4
  memory_gb     = 16
  disk_gb       = 80
  desired_nodes = 3
}

# Autoscaling pool: the vCPU/memory budget makes it autoscaling (billed hourly per node).
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

output "kubeconfig" {
  value     = data.aether_kubeconfig.prod.content
  sensitive = true
}

data "aether_kubeconfig" "prod" {
  cluster_id = aether_cluster.prod.id
}
```

`templates/index.md.tmpl`:

```
---
page_title: "Aether Provider"
description: Manage Aether Kubernetes clusters, access and registry with Terraform or OpenTofu.
---

# Aether Provider

Works with Terraform 1.x and OpenTofu 1.6+.

## Prerequisite: an API token

Create a token in the Aether portal (Organization → API tokens). The token belongs to one organization; the provider uses that organization for every resource.

{{ .SchemaMarkdown }}

## Example

{{ tffile "examples/provider/provider.tf" }}
```

`.goreleaser.yml` (standard HashiCorp scaffolding layout):

```yaml
version: 2
before:
  hooks: [go mod tidy]
builds:
  - env: [CGO_ENABLED=0]
    mod_timestamp: '{{ .CommitTimestamp }}'
    flags: [-trimpath]
    ldflags: ['-s -w -X main.version={{.Version}}']
    goos: [freebsd, windows, linux, darwin]
    goarch: [amd64, '386', arm, arm64]
    ignore:
      - { goos: darwin, goarch: '386' }
      - { goos: darwin, goarch: arm }
      - { goos: windows, goarch: arm }
      - { goos: windows, goarch: arm64 }
    binary: '{{ .ProjectName }}_v{{ .Version }}'
archives:
  - formats: [zip]
    name_template: '{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}'
checksum:
  extra_files:
    - glob: 'terraform-registry-manifest.json'
      name_template: '{{ .ProjectName }}_{{ .Version }}_manifest.json'
  name_template: '{{ .ProjectName }}_{{ .Version }}_SHA256SUMS'
  algorithm: sha256
signs:
  - artifacts: checksum
    args: ["--batch", "--local-user", "{{ .Env.GPG_FINGERPRINT }}", "--output", "${signature}", "--detach-sign", "${artifact}"]
release:
  extra_files:
    - glob: 'terraform-registry-manifest.json'
      name_template: '{{ .ProjectName }}_{{ .Version }}_manifest.json'
changelog:
  disable: true
```

`terraform-registry-manifest.json`:

```json
{ "version": 1, "metadata": { "protocol_versions": ["6.0"] } }
```

`.github/workflows/build-test.yml` (pin actions to full SHAs — resolve with `gh api repos/<owner>/<repo>/commits/<tag>`; the `<sha>` placeholders below are resolved by the coder):

```yaml
name: Build & Test
on:
  pull_request:
  push: { branches: [main] }
permissions: { contents: read }
jobs:
  test:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        engine: [terraform, tofu]
    steps:
      - uses: actions/checkout@<sha>   # v4
        with: { persist-credentials: false }
      - uses: actions/setup-go@<sha>   # v5
        with: { go-version-file: go.mod }
      - uses: hashicorp/setup-terraform@<sha>   # v3
        if: matrix.engine == 'terraform'
        with: { terraform_wrapper: false }
      - uses: opentofu/setup-opentofu@<sha>     # v1
        if: matrix.engine == 'tofu'
        with: { tofu_wrapper: false }
      - run: go generate ./... && git diff --exit-code -- internal/client
      - run: go vet ./... && test -z "$(gofmt -l .)"
      - run: go test ./... -count=1 -race
        env:
          TF_ACC_TERRAFORM_PATH: ${{ matrix.engine == 'tofu' && '/usr/local/bin/tofu' || '' }}
          TF_ACC_PROVIDER_HOST: ${{ matrix.engine == 'tofu' && 'registry.opentofu.org' || '' }}
      - run: go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate --provider-name aether && git diff --exit-code -- docs
```

`release.yml`: on `push: tags: ['v*']`; `permissions: contents: write`; checkout (fetch-depth 0), setup-go, `crazy-max/ghaction-import-gpg` with `GPG_PRIVATE_KEY`/`PASSPHRASE` secrets, `goreleaser/goreleaser-action` `release --clean` with `GPG_FINGERPRINT` from the import step and `GITHUB_TOKEN`. `acceptance.yml`: `workflow_dispatch` + `schedule: '0 3 * * *'`; secrets `AETHER_DEV_API_TOKEN`; matrix `[terraform, tofu]`; runs `make testacc` with `AETHER_ENDPOINT=https://dev.api.aetherplatform.cloud`; `concurrency: acceptance` (serial).

- [ ] **Step 1 [CODER]:** write examples, templates, workflows, goreleaser, manifest, README (install for both engines; token prerequisite; dev endpoint). Run `make docs`; commit `docs/`.
- [ ] **Step 2 [TEST-RUNNER]:** `make lint test docs && git diff --exit-code docs`; `goreleaser check`; `actionlint` if available.
- [ ] **Step 3 [CODER]:** commit `chore: docs generation, CI (terraform + tofu), GoReleaser release, registry manifest`.

---

### Task 17: aether-docs Terraform/OpenTofu section + PRD §7.2 correction

**Files (aether-docs repo, worktree off `origin/main`):** Create `site/src/content/docs/guides/terraform.md` (getting started: install for Terraform and OpenTofu, create an API token in the portal, provider block, cluster + pool example, import your organization, dev endpoint note); Modify `docs/PRD.md` §7.2.2–7.2.4 (resource table: drop `aether_api_token`, `machine_tier`; region `eu-fi`; provider-level org; live field names; note that `readOnly` is not what marks computed fields — the provider decides); re-extract §7 into `aether-api/docs/PRD.md` and `terraform-provider-aether/docs/PRD.md` per CLAUDE.md's PRD workflow; `docs/progress.md` + `progress.html` session row.

Also in PRD §7.2: terminal states per the live enum (`running`, `degraded`, `failed`; `deleting` transient; no `deleted`), the `registry_` naming of robot accounts (never expose the underlying technology), and `aether_kubeconfig` = the admin kubeconfig (privileged), not "OIDC".

- [ ] **Step 1 [CODER]:** edits + commits in each repo (docs PR to aether-docs; PRD-mirror PRs to aether-api and the provider). **Step 2 [TEST-RUNNER]:** `grep -rn 'machine_tier\|eu-de\|aether_api_token' aether-docs/docs/PRD.md terraform-provider-aether/docs/PRD.md` → no output. **Step 3:** PRs.

---

### Task 18: Release `v0.1.0` and registry publication (owner-gated)

- [ ] **Step 1 [CODER]:** after Tasks 1–17 are merged to `main`: `git tag -a v0.1.0 -m "terraform-provider-aether v0.1.0" && git push origin v0.1.0` → release workflow → verify the GitHub Release has zips, `SHA256SUMS`, `.sig`, manifest.
- [ ] **Step 2 [OWNER]:** flip the repo public; on registry.terraform.io sign in with the plane-shift org, add the GPG public key, publish `plane-shift/aether`; submit the OpenTofu registry manifest + key.
- [ ] **Step 3 [TEST-RUNNER]:** from a scratch dir: `terraform init` and `tofu init` with `source = "plane-shift/aether"` resolve `0.1.0`; `terraform providers` lists it.
- [ ] **Step 4 [CODER]:** CLAUDE.md line + memory: provider shipped, address, registries, dev acceptance secret name.

---

## Self-Review

**Spec coverage:** §1 rulings → Global Constraints + Tasks 1 (provider-level org, token prerequisite), 16/18 (public repo, both registries), 15 (dev acceptance). §2 layout → File Structure. §3.1–3.5 → Tasks 1, 2, 4, 5, 3. §4.1–4.10 → Tasks 6, 7, 8, 9, 9, 10, 11, 11, 11, 12. §5 data sources → Tasks 13–14 (all 24 named). §5.1 timeouts → Tasks 6/7 defaults. §6 error table → Task 3 (+ 404/failed/timeout in Tasks 4, 6). §7 testing layers 1–4 → Tasks 1–14 (unit), 15 (acceptance, both engines), 2 (contract), 16 (CI matrix). §8 → Task 16 (CI/release/docs), Task 18 (registries). §9 owner items → Task 15 prerequisites, Task 18 Step 2. PRD drift → Task 17.

**Placeholder scan:** Tasks 7–12 and 14 give schemas, behaviours and test lists but reference Task 6/13's complete code as the pattern; every field, endpoint and rule they need is spelled out in their own text, and the generated client supplies the types. Action SHAs in Task 16 are to be resolved by the coder (a literal SHA cannot be known in advance) — stated explicitly. No TBD/TODO.

**Type consistency:** `providerData{Client, OrgID}` and `fromProviderData` (Task 1) used by all resources/data sources; `apierr.Parse/ToDiagnostics/IsNotFound` (Task 3) used in Tasks 6–14; `wait.ForStatus/ForGone/Options` (Task 4) used in Tasks 6–8, 11; `versions.K8sVersionType/Value, SemanticallyEqual, IsForward` (Task 5) used in Task 6; `collectAll` (Task 13) used in Tasks 13–14; `deref/optString` helpers (Task 6, `helpers.go`) used everywhere; node-pool import id `<cluster_id>/<pool_id>` documented in Task 7 and the Task 16 docs.
