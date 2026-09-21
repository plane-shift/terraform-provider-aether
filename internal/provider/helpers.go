package provider

import (
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/apierr"
)

// deref returns the string p points at, or "" for nil. For places where the
// API's "absent" and "empty" mean the same thing (a status message); where
// they differ, use optString.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// optString maps an optional API string onto a Terraform value: absent
// (nil, i.e. omitted from the response) becomes null, never "". A computed
// attribute that reads "" where the API said nothing would claim a value the
// API never reported.
func optString(p *string) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(*p)
}

// addAPIError appends the diagnostics for a non-success API answer. It is the
// one spelling of "parse the error envelope, then map it onto attributes":
// status, header and body come straight from the generated response (the
// request id lives in the RESPONSE header, never the body, which is why the
// header is passed). attrPrefix is path.Empty() for top-level attributes, or
// the attribute a whole-request error belongs to.
func addAPIError(diags *diag.Diagnostics, status int, header http.Header, body []byte, attrPrefix path.Path) {
	apierr.ToDiagnostics(apierr.Parse(status, header, body), diags, attrPrefix)
}

// describeNonJSON describes a success answer whose body the generated client
// could not decode into its typed field (the JSONnnn pointer is nil): method,
// URL, status line and content type. A success without a usable body is NOT a
// reading of the resource - an id or status taken from it would be a zero
// value - so callers turn this into an error or a diagnostic, never into state.
func describeNonJSON(hr *http.Response, status string) string {
	return fmt.Sprintf("%s %s returned %s with content type %q, not JSON",
		hr.Request.Method, hr.Request.URL, status, hr.Header.Get("Content-Type"))
}
