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
