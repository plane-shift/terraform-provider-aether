package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// The generated client types every organization id as openapi_types.UUID
// (a [16]byte, google/uuid.UUID), and providerData.OrgID is expected to hold
// the canonical UUID text the client round-trips it as. These constants are
// real UUID text, not opaque strings like "org-1", so they exercise the same
// parse/format path production traffic does.
const (
	orgAcme      = "11111111-1111-4111-8111-111111111111"
	orgSecondary = "22222222-2222-4222-8222-222222222222"
	orgUnseen    = "33333333-3333-4333-8333-333333333333" // never registered on the fake
)

func configure(t *testing.T, endpoint, token, orgID string) (provider.ConfigureResponse, *providerData) {
	t.Helper()
	// A developer's own shell may export any of these three for unrelated
	// reasons (a real terraform run, a different project). Configure treats
	// the env vars as a fallback, so leaving them alone here would let a
	// value that has nothing to do with THIS test decide which path
	// Configure takes - non-deterministic, and only on some machines.
	t.Setenv("AETHER_API_TOKEN", "")
	t.Setenv("AETHER_ENDPOINT", "")
	t.Setenv("AETHER_ORGANIZATION_ID", "")
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
	callConfigure(t, p, req, &resp)
	pd, _ := resp.ResourceData.(*providerData)
	return resp, pd
}

// callConfigure runs Configure guarded by a recover, so a production panic
// surfaces as an ordinary failing test instead of crashing the whole test
// binary - which would also swallow every other test's result in this run.
func callConfigure(t *testing.T, p provider.Provider, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Configure panicked: %v", r)
		}
	}()
	p.Configure(context.Background(), req, resp)
}

// configureRaw is configure's raw-tftypes.Value variant: it lets a test set
// api_token or organization_id to tftypes.UnknownValue, which configure's
// string-argument signature cannot express (an unknown value is not a Go
// string).
func configureRaw(t *testing.T, api *fakeapi.Server, apiToken, orgID tftypes.Value) (provider.ConfigureResponse, *providerData) {
	t.Helper()
	// Same rationale as configure(): an unknown attribute's ValueString() is
	// "", indistinguishable from a leaked env fallback unless the env is
	// pinned to empty for the duration of this call.
	t.Setenv("AETHER_API_TOKEN", "")
	t.Setenv("AETHER_ENDPOINT", "")
	t.Setenv("AETHER_ORGANIZATION_ID", "")
	p := New("test")()
	schemaResp := provider.SchemaResponse{}
	p.Schema(context.Background(), provider.SchemaRequest{}, &schemaResp)
	raw := tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"api_token": tftypes.String, "endpoint": tftypes.String, "organization_id": tftypes.String,
	}}, map[string]tftypes.Value{
		"api_token": apiToken, "endpoint": tftypes.NewValue(tftypes.String, api.URL()), "organization_id": orgID,
	})
	req := provider.ConfigureRequest{Config: tfsdk.Config{Raw: raw, Schema: schemaResp.Schema}}
	resp := provider.ConfigureResponse{}
	callConfigure(t, p, req, &resp)
	pd, _ := resp.ResourceData.(*providerData)
	return resp, pd
}

// anyDetailMentionsUnknown reports whether any error diagnostic's detail
// explains that the value is unknown / not yet known, as opposed to absent.
func anyDetailMentionsUnknown(errs diag.Diagnostics) bool {
	for _, e := range errs {
		d := strings.ToLower(e.Detail())
		if strings.Contains(d, "unknown") || strings.Contains(d, "not yet known") {
			return true
		}
	}
	return false
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
	// The typed and the text form are set on the same path and must agree;
	// nothing reads orgID until a resource does, so pin the invariant here.
	if pd.orgID.String() != pd.OrgID {
		t.Fatalf("orgID = %s, OrgID = %s: the two forms disagree", pd.orgID, pd.OrgID)
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
	if pd.orgID.String() != pd.OrgID {
		t.Fatalf("orgID = %s, OrgID = %s: the two forms disagree", pd.orgID, pd.OrgID)
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
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics.Errors()[0].Detail(), "organization_id") {
		t.Fatalf("expected an organization_id error, got %v", resp.Diagnostics)
	}
}

// TestConfigure_ExplicitOrganizationAuthFailureNamesTheToken pins a
// review-confirmed defect: on ANY non-200 for GET /organizations/{id},
// Configure unconditionally blames organization_id ("organization_id is not
// visible to this token"), even for a 401 - where the actual problem is the
// bearer token, not the id. That wording also actively misleads: the token
// WAS rejected, so "not visible to this token" implies the id is wrong when
// the token itself is.
func TestConfigure_ExplicitOrganizationAuthFailureNamesTheToken(t *testing.T) {
	api := fakeapi.New(t)
	api.Handlers["GET /organizations/"+orgAcme] = func(w http.ResponseWriter, r *http.Request) {
		fakeapi.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "invalid token"}})
	}
	resp, _ := configure(t, api.URL(), api.Token(), orgAcme)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error")
	}
	got := resp.Diagnostics.Errors()[0].Detail()
	if !strings.Contains(got, "api_token") {
		t.Fatalf("detail = %q, want a hint naming api_token - a 401 means the TOKEN was rejected", got)
	}
	if strings.Contains(strings.ToLower(got), "not visible") {
		t.Fatalf("detail = %q, must not claim the organization is not visible when the token itself was rejected", got)
	}
}

// TestConfigure_ExplicitOrganizationServiceUnavailableNamesTheStatusNotTheOrg
// is TestConfigure_ExplicitOrganizationAuthFailureNamesTheToken's sibling for
// a transient server failure: a 503 is neither a bad token nor a bad
// organization_id, and today's fixed wording ("...omit organization_id to use
// it") blames organization_id regardless.
func TestConfigure_ExplicitOrganizationServiceUnavailableNamesTheStatusNotTheOrg(t *testing.T) {
	api := fakeapi.New(t)
	api.Handlers["GET /organizations/"+orgAcme] = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	resp, _ := configure(t, api.URL(), api.Token(), orgAcme)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error")
	}
	got := resp.Diagnostics.Errors()[0].Detail()
	if strings.Contains(got, "organization_id") {
		t.Fatalf("detail = %q, must not blame organization_id for a 503", got)
	}
	if !strings.Contains(got, "503") && !strings.Contains(strings.ToLower(got), "unavailable") {
		t.Fatalf("detail = %q, want it to name the status or unavailability", got)
	}
}

// TestConfigure_DoesNotFollowRedirectsWithTheToken: an API client has no
// business following a 302 - and retryTransport unconditionally re-sets the
// bearer token's Authorization header on EVERY request it sees, including a
// redirected one to a different host, so following redirects here is a token
// leak, not just wasted work. Two real httptest.Server instances both bind to
// 127.0.0.1: Go's stdlib only strips Authorization on a REDIRECT ACROSS
// DIFFERENT HOSTNAMES (net/http's shouldCopyHeaderOnRedirect compares
// hostname only, ignoring port) - same hostname, different port counts as
// "same domain" to the stdlib, so its own protection does not apply here even
// before considering that retryTransport re-adds the header regardless.
func TestConfigure_DoesNotFollowRedirectsWithTheToken(t *testing.T) {
	var secondHostHits int32
	var secondHostAuth string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHostHits, 1)
		secondHostAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	api := fakeapi.New(t)
	api.Handlers["GET /organizations"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", second.URL+"/steal-the-token")
		w.WriteHeader(http.StatusFound)
	}
	resp, _ := configure(t, api.URL(), api.Token(), "")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a diagnostic error rather than silently following the redirect")
	}
	if atomic.LoadInt32(&secondHostHits) != 0 {
		t.Fatalf("the redirect target was hit %d time(s); the client must refuse to follow the redirect at all", secondHostHits)
	}
	if secondHostAuth != "" {
		t.Fatalf("Authorization leaked to the redirect target: %q", secondHostAuth)
	}
}

// TestConfigure_ExplicitOrganizationMustBeAUUID is a new expectation beyond
// the task brief: the generated client types organization_id as
// openapi_types.UUID, so a syntactically-invalid id must be rejected as an
// attribute error before any API call is made (never surfaced only as a
// downstream JSON/path-encoding failure).
func TestConfigure_ExplicitOrganizationMustBeAUUID(t *testing.T) {
	api := fakeapi.New(t)
	api.AddOrg(orgAcme, "Acme", "acme")
	resp, _ := configure(t, api.URL(), api.Token(), "not-a-uuid")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an organization_id error")
	}
	if got := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(got, "organization_id") {
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
	if got := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(got, "2 organizations") || !strings.Contains(got, "organization_id") {
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
	if got := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(got, "api_token") {
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

// TestConfigure_NonJSON200IsADiagnosticNotAPanic pins a review-confirmed
// defect: ParseListOrganizationsResponse (internal/client/client.gen.go) only
// populates JSON200 when the response Content-Type contains "json"; on a 200
// with any other Content-Type, JSON200 stays nil, and today's Configure dereferences
// list.JSON200.Items unconditionally - a nil-pointer panic, not a diagnostic.
// The function name drops the hyphen the review brief used
// ("Non-JSON200..."), which is not a legal Go identifier.
func TestConfigure_NonJSON200IsADiagnosticNotAPanic(t *testing.T) {
	api := fakeapi.New(t)
	api.Handlers["GET /organizations"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>not an API response</body></html>"))
	}
	resp, _ := configure(t, api.URL(), api.Token(), "")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a diagnostic error for a non-JSON 200 response, got none (and no panic)")
	}
}

// TestConfigure_UnknownValuesAreRejected pins a review-confirmed defect: an
// unknown api_token or organization_id (e.g. computed from a resource not yet
// applied) reads through StringValue.ValueString() as "", which is
// indistinguishable from an omitted value. Today that means an unknown
// api_token is reported as "Missing API token" (wrong diagnostic - the value
// isn't missing, it isn't known YET) and an unknown organization_id silently
// falls through to auto-resolution, reaching the API before the value the
// practitioner actually configured is even known.
func TestConfigure_UnknownValuesAreRejected(t *testing.T) {
	t.Run("api_token", func(t *testing.T) {
		api := fakeapi.New(t)
		api.AddOrg(orgAcme, "Acme", "acme")
		resp, _ := configureRaw(t, api,
			tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
			tftypes.NewValue(tftypes.String, orgAcme),
		)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an attribute error for an unknown api_token")
		}
		for _, e := range resp.Diagnostics.Errors() {
			if e.Summary() == "Missing API token" {
				t.Fatalf("got the missing-token diagnostic for a value that is unknown (not yet known), not absent: %v", e)
			}
		}
		if !anyDetailMentionsUnknown(resp.Diagnostics.Errors()) {
			t.Fatalf("expected a diagnostic explaining api_token is not yet known, got %v", resp.Diagnostics)
		}
	})
	t.Run("organization_id", func(t *testing.T) {
		api := fakeapi.New(t)
		api.AddOrg(orgAcme, "Acme", "acme")
		resp, _ := configureRaw(t, api,
			tftypes.NewValue(tftypes.String, api.Token()),
			tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		)
		if !resp.Diagnostics.HasError() {
			t.Fatal("expected an attribute error for an unknown organization_id")
		}
		if !anyDetailMentionsUnknown(resp.Diagnostics.Errors()) {
			t.Fatalf("expected a diagnostic explaining organization_id is not yet known, got %v", resp.Diagnostics)
		}
		if got := api.Requests(); len(got) != 0 {
			t.Fatalf("expected zero API calls while organization_id is unknown (must not fall through to auto-resolution), got %v", got)
		}
	})
}

// TestConfigure_DoesNotLeakADevelopersExportedToken pins why the configure
// helper must reset AETHER_API_TOKEN/AETHER_ENDPOINT/AETHER_ORGANIZATION_ID
// to "" itself: without that, a value a developer happens to have exported
// in their own shell silently satisfies firstNonEmpty() instead of the
// blank api_token this test passes, and the assertion below can no longer
// tell "correctly rejected as absent" from "accidentally worked because of
// whatever was in the environment when `go test` ran".
func TestConfigure_DoesNotLeakADevelopersExportedToken(t *testing.T) {
	api := fakeapi.New(t)
	t.Setenv("AETHER_API_TOKEN", "aether_"+strings.Repeat("z", 64)) // wrong, but non-empty
	resp, _ := configure(t, api.URL(), "", "")
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error")
	}
	if got := resp.Diagnostics.Errors()[0].Summary(); got != "Missing API token" {
		t.Fatalf("summary = %q, want %q - a leaked exported AETHER_API_TOKEN reached the API instead of being rejected as absent", got, "Missing API token")
	}
}
