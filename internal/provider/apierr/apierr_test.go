package apierr

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

// headerWithRequestID builds the X-Request-Id response header Parse reads the
// request id from. Measured against aether-api (origin/main 8e9045a,
// internal/api/middleware/requestid.go): the id is a response HEADER set on
// every route, never a field in the JSON body.
func headerWithRequestID(id string) http.Header {
	h := http.Header{}
	if id != "" {
		h.Set("X-Request-Id", id)
	}
	return h
}

func TestParse_ValidationErrorsCarryFields(t *testing.T) {
	// Measured against aether-api internal/api/handler/response.go and the
	// vendored ErrorResponse/ValidationError schemas in api/openapi.yaml:
	// field errors are error.details[], never a top-level "errors" list, and
	// the request id is never a body field.
	body := []byte(`{"error":{"code":"validation_error","message":"invalid request","details":[{"field":"k8s_version","code":"not_offered","message":"1.31 is not offered"},{"field":"name","code":"required","message":"name is required"}]}}`)
	e := Parse(http.StatusUnprocessableEntity, headerWithRequestID("req-1"), body)
	if e.Status != 422 || e.Code != "validation_error" || e.RequestID != "req-1" {
		t.Fatalf("%+v", e)
	}
	if len(e.Fields) != 2 || e.Fields[0].Field != "k8s_version" {
		t.Fatalf("fields = %+v", e.Fields)
	}
}

func TestParse_OldWrongEnvelopeShapeYieldsNoFieldErrors(t *testing.T) {
	// The shape the original brief (and this package, before this
	// correction) assumed: field errors under a top-level "errors" list
	// instead of error.details, and the id inside the body instead of the
	// X-Request-Id header. The real API never sends this. A body shaped this
	// way must not be quietly accepted as valid field data — that is exactly
	// how the wrong contract shipped once; it must never pass silently again.
	body := []byte(`{"error":{"code":"validation_error","message":"invalid request","request_id":"req-1"},"errors":[{"field":"k8s_version","code":"not_offered","message":"1.31 is not offered"}]}`)
	e := Parse(http.StatusUnprocessableEntity, http.Header{}, body)
	if len(e.Fields) != 0 {
		t.Fatalf("fields = %+v, want none — 422 field errors live under error.details, not a top-level errors list", e.Fields)
	}
	if e.RequestID != "" {
		t.Fatalf("RequestID = %q, want empty — the request id is the X-Request-Id header, never a body field", e.RequestID)
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

func TestParse_NoRequestIDHeaderLeavesRequestIDEmpty(t *testing.T) {
	e := Parse(http.StatusInternalServerError, http.Header{}, []byte(`{"error":{"code":"internal","message":"boom"}}`))
	if e.RequestID != "" {
		t.Fatalf("RequestID = %q, want empty when the response carried no X-Request-Id header", e.RequestID)
	}
}

// TestParse_NilHeaderIsSafe: header.Get on a nil http.Header must not panic
// (net/http's Header.Get is documented nil-safe, and Parse's own doc comment
// already claims this), and must leave RequestID empty exactly like an
// explicitly-empty header would.
func TestParse_NilHeaderIsSafe(t *testing.T) {
	e := Parse(http.StatusInternalServerError, nil, []byte(`{"error":{"code":"internal","message":"boom"}}`))
	if e.RequestID != "" {
		t.Fatalf("RequestID = %q, want empty for a nil header", e.RequestID)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(Parse(404, http.Header{}, []byte(`{"error":{"code":"not_found","message":"gone"}}`))) {
		t.Fatal("404 must be IsNotFound")
	}
	if IsNotFound(Parse(500, http.Header{}, nil)) {
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

func TestToDiagnostics_NestedFieldPathAddressesNestedAttribute(t *testing.T) {
	e := Parse(422, http.Header{}, []byte(`{"error":{"code":"validation_error","message":"invalid","details":[{"field":"node_pools.name","code":"required","message":"name is required"}]}}`))
	var d diag.Diagnostics
	ToDiagnostics(e, &d, path.Empty())
	if len(d) != 1 {
		t.Fatalf("diags = %v", d)
	}
	ad, ok := d[0].(diag.DiagnosticWithPath)
	if !ok || !ad.Path().Equal(path.Root("node_pools").AtName("name")) {
		t.Fatalf("expected an attribute error at node_pools.name, got %v", d[0])
	}
}

func TestToDiagnostics_AuthIsOneProviderHint(t *testing.T) {
	var d diag.Diagnostics
	ToDiagnostics(Parse(403, http.Header{}, []byte(`{"error":{"code":"forbidden","message":"nope"}}`)), &d, path.Empty())
	if len(d) != 1 || !strings.Contains(d[0].Detail(), "api_token") {
		t.Fatalf("diags = %v", d)
	}
}

func TestToDiagnostics_RequestIDIsSurfaced(t *testing.T) {
	var d diag.Diagnostics
	ToDiagnostics(Parse(500, headerWithRequestID("req-9"), []byte(`{"error":{"code":"internal","message":"boom"}}`)), &d, path.Empty())
	if !strings.Contains(d[0].Detail(), "req-9") {
		t.Fatalf("detail = %q", d[0].Detail())
	}
}

func TestToDiagnostics_NoRequestIDHeaderOmitsSuffix(t *testing.T) {
	var d diag.Diagnostics
	ToDiagnostics(Parse(500, http.Header{}, []byte(`{"error":{"code":"internal","message":"boom"}}`)), &d, path.Empty())
	if strings.Contains(d[0].Detail(), "request id") {
		t.Fatalf("detail = %q, want no request id suffix when the response carried no X-Request-Id header", d[0].Detail())
	}
}

// TestToDiagnostics_NilHeaderParseOmitsRequestIDSuffix is
// TestToDiagnostics_NoRequestIDHeaderOmitsSuffix's nil-header sibling: a
// literal nil (not just an empty http.Header{}) must flow through Parse and
// ToDiagnostics without panicking and without ever surfacing a request id.
func TestToDiagnostics_NilHeaderParseOmitsRequestIDSuffix(t *testing.T) {
	var d diag.Diagnostics
	ToDiagnostics(Parse(500, nil, []byte(`{"error":{"code":"internal","message":"boom"}}`)), &d, path.Empty())
	if strings.Contains(d[0].Detail(), "request id") {
		t.Fatalf("detail = %q, want no request id suffix when Parse was given a nil header", d[0].Detail())
	}
}

// ---------------------------------------------------------------- organization role enforcement

// The three refusals of aether-api's organization role enforcement, with the
// messages the API sends (aether-api internal/api/middleware/org_authz.go and
// internal/api/handler/member.go).
const (
	insufficientRoleBody  = `{"error":{"code":"insufficient_role","message":"Your role in this organization does not permit this action"}}`
	tokenNotPermittedBody = `{"error":{"code":"token_not_permitted","message":"API tokens may not perform this action — use an owner or admin login"}}`
	lastOwnerBody         = `{"error":{"code":"last_owner","message":"The organization's last owner cannot be removed or demoted — add another owner first"}}`
)

func oneDiag(t *testing.T, status int, h http.Header, body string) diag.Diagnostic {
	t.Helper()
	var d diag.Diagnostics
	ToDiagnostics(Parse(status, h, []byte(body)), &d, path.Empty())
	if len(d) != 1 {
		t.Fatalf("diags = %v, want exactly one", d)
	}
	if d[0].Severity() != diag.SeverityError {
		t.Fatalf("severity = %v, want error", d[0].Severity())
	}
	return d[0]
}

// Fails if one of the three role-enforcement refusals loses its dedicated
// summary (e.g. falls back to the generic auth or Conflict diagnostic), if the
// detail drops the API's own message, or if a present request id is not
// surfaced.
func TestToDiagnostics_RoleEnforcementRefusalsHaveDedicatedSummaries(t *testing.T) {
	for _, c := range []struct {
		name, body, summary, apiMessage string
		status                          int
	}{
		{"403 insufficient_role", insufficientRoleBody,
			"The API token's organization role does not allow this operation",
			"Your role in this organization does not permit this action", http.StatusForbidden},
		{"403 token_not_permitted", tokenNotPermittedBody,
			"API tokens cannot perform this operation",
			"API tokens may not perform this action — use an owner or admin login", http.StatusForbidden},
		{"409 last_owner", lastOwnerBody,
			"Cannot remove the organization's last owner",
			"The organization's last owner cannot be removed or demoted — add another owner first", http.StatusConflict},
	} {
		t.Run(c.name+"/with request id", func(t *testing.T) {
			d := oneDiag(t, c.status, headerWithRequestID("req-42"), c.body)
			if d.Summary() != c.summary {
				t.Fatalf("summary = %q, want %q", d.Summary(), c.summary)
			}
			if !strings.Contains(d.Detail(), c.apiMessage) {
				t.Fatalf("detail = %q, want it to carry the API message %q", d.Detail(), c.apiMessage)
			}
			if !strings.Contains(d.Detail(), "(request id req-42)") {
				t.Fatalf("detail = %q, want the request id surfaced", d.Detail())
			}
		})
		t.Run(c.name+"/without request id", func(t *testing.T) {
			d := oneDiag(t, c.status, http.Header{}, c.body)
			if d.Summary() != c.summary {
				t.Fatalf("summary = %q, want %q", d.Summary(), c.summary)
			}
			if !strings.Contains(d.Detail(), c.apiMessage) {
				t.Fatalf("detail = %q, want it to carry the API message %q", d.Detail(), c.apiMessage)
			}
			if strings.Contains(d.Detail(), "(request id") {
				t.Fatalf("detail = %q, want no request id suffix when the response carried no X-Request-Id header", d.Detail())
			}
		})
	}
}

// Fails if a role-enforcement 403 still tells the practitioner to check
// api_token: the token was accepted, so that hint points at the wrong thing.
func TestToDiagnostics_RoleEnforcement403DetailsDoNotBlameTheToken(t *testing.T) {
	for name, body := range map[string]string{"insufficient_role": insufficientRoleBody, "token_not_permitted": tokenNotPermittedBody} {
		t.Run(name, func(t *testing.T) {
			d := oneDiag(t, http.StatusForbidden, headerWithRequestID("req-1"), body)
			if strings.Contains(d.Detail(), "api_token") {
				t.Fatalf("detail = %q, must not send the practitioner to check api_token", d.Detail())
			}
		})
	}
}

// Fails if the dedicated branches widen to codes they do not own: any other
// 403 and every 401 keep the generic diagnostic with the api_token hint.
func TestToDiagnostics_Other401And403KeepTheGenericTokenHint(t *testing.T) {
	for _, c := range []struct {
		name, body string
		status     int
	}{
		{"403 forbidden", `{"error":{"code":"forbidden","message":"no access to this organization for this token"}}`, http.StatusForbidden},
		{"401 unauthorized", `{"error":{"code":"unauthorized","message":"invalid token"}}`, http.StatusUnauthorized},
		// The code alone does not select the dedicated diagnostic: only a 403
		// carrying it does.
		{"401 token_not_permitted", tokenNotPermittedBody, http.StatusUnauthorized},
		{"401 insufficient_role", insufficientRoleBody, http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := oneDiag(t, c.status, http.Header{}, c.body)
			if d.Summary() != "Aether API rejected the request" {
				t.Fatalf("summary = %q, want the generic %q", d.Summary(), "Aether API rejected the request")
			}
			if !strings.Contains(d.Detail(), "api_token") {
				t.Fatalf("detail = %q, want the api_token hint", d.Detail())
			}
		})
	}
}

// Fails if the last_owner branch swallows every 409, or if a last_owner code
// on another status is rendered as the last-owner refusal.
func TestToDiagnostics_Other409KeepsConflict(t *testing.T) {
	d := oneDiag(t, http.StatusConflict, http.Header{}, `{"error":{"code":"conflict","message":"Member already exists"}}`)
	if d.Summary() != "Conflict" || d.Detail() != "Member already exists" {
		t.Fatalf("diag = %q / %q, want Conflict / the API message", d.Summary(), d.Detail())
	}
	d = oneDiag(t, http.StatusUnprocessableEntity, http.Header{}, lastOwnerBody)
	if d.Summary() == "Cannot remove the organization's last owner" {
		t.Fatalf("a 422 carrying last_owner was rendered as the 409 last-owner refusal: %q", d.Detail())
	}
}
