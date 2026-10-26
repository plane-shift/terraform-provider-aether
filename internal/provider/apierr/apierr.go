// Package apierr turns Aether API error responses into Terraform diagnostics.
//
// The wire format is measured, not assumed - aether-api
// internal/api/handler/response.go and internal/api/middleware/requestid.go at
// 8e9045a, the SHA in OPENAPI_REF (re-checked there; the definitions are
// byte-identical to 8f03fbb, where they were first measured):
//
//	{"error":{"code":"...","message":"...","details":[{"field","code","message"}]}}
//
// Two properties of that shape have already been got wrong once. Field errors
// live under error.details, NOT a top-level "errors" list. And the request id
// is the X-Request-ID RESPONSE HEADER, which the middleware sets on every
// route - it is never a field in the body. Parse therefore takes the header,
// and a body shaped like the old guess yields no field errors rather than
// being quietly accepted.
//
// A field error has to reach the practitioner as an error ON THAT ATTRIBUTE -
// a single provider-level error naming the field in prose makes Terraform point
// at the resource block instead of the line that is wrong.
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

// error.code values the provider renders with a dedicated diagnostic. They
// come from the organization role enforcement in aether-api (the
// Authorization section of api/openapi.yaml).
const (
	// CodeInsufficientRole: 403, the caller's organization role is below the
	// operation's minimum.
	CodeInsufficientRole = "insufficient_role"
	// CodeTokenNotPermitted: 403, the operation refuses API-token callers.
	CodeTokenNotPermitted = "token_not_permitted"
	// CodeLastOwner: 409, the request would remove or demote the
	// organization's last owner.
	CodeLastOwner = "last_owner"
)

// FieldError is one entry of the 422 error.details list.
type FieldError struct {
	Field   string
	Code    string
	Message string
}

// Error is a parsed API error response. It is always non-nil from Parse: a body
// that is not the documented envelope still produces an error carrying the
// status, because "the server said no and we could not read why" is itself the
// diagnosis.
type Error struct {
	Status  int
	Code    string
	Message string
	// RequestID comes from the X-Request-ID response header, not the body.
	RequestID string
	// Fields are the 422 error.details entries, empty for every other status.
	Fields []FieldError
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("aether api: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("aether api: %d: %s", e.Status, e.Message)
}

// envelope matches ErrorResponse / ErrorBody in aether-api's
// internal/api/handler/response.go, which is what the vendored
// api/openapi.yaml describes.
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
// where the request id lives; a nil header is safe. It never returns nil.
func Parse(status int, header http.Header, body []byte) *Error {
	// http.Header.Get canonicalises the key, so the wire's X-Request-ID
	// matches; a nil Header returns "" rather than panicking.
	e := &Error{Status: status, RequestID: header.Get("X-Request-Id")}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || (env.Error.Message == "" && len(env.Error.Details) == 0) {
		// Not the documented envelope (an HTML 502 from a proxy, an empty
		// body). Keep the status text plus a bounded slice of whatever did
		// arrive - discarding it entirely would leave nothing to debug with.
		e.Message = strings.TrimSpace(http.StatusText(status) + " " + Truncate(string(body), 200))
		return e
	}
	e.Code, e.Message = env.Error.Code, env.Error.Message
	for _, f := range env.Error.Details {
		e.Fields = append(e.Fields, FieldError{Field: f.Field, Code: f.Code, Message: f.Message})
	}
	return e
}

// IsNotFound reports whether err is an API error with status 404. Resources use
// it to remove themselves from state instead of erroring on a Read.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// ToDiagnostics appends diagnostics for err. attrPrefix is prepended to field
// paths (path.Empty() for top-level attributes), so a nested block can map the
// API's field names onto its own address space.
func ToDiagnostics(err error, diags *diag.Diagnostics, attrPrefix path.Path) {
	var e *Error
	if !errors.As(err, &e) {
		diags.AddError("Aether API request failed", err.Error())
		return
	}
	rid := ""
	if e.RequestID != "" {
		rid = " (request id " + e.RequestID + ")"
	}
	switch {
	// Organization role enforcement (aether-api, Authorization section of
	// api/openapi.yaml): these two 403 codes mean the token was ACCEPTED and
	// the operation itself is not open to it, so "check api_token" would send
	// the practitioner after the wrong thing. Matched on error.code; every
	// other 401/403 keeps the token hint below.
	case e.Status == http.StatusForbidden && e.Code == CodeInsufficientRole:
		diags.AddError("The API token's organization role does not allow this operation",
			fmt.Sprintf("%s%s. Use an API token with the Admin role (Settings → API Tokens); changing a member's role is open to no token and must be done in the portal, signed in as an organization owner.", e.Message, rid))
	case e.Status == http.StatusForbidden && e.Code == CodeTokenNotPermitted:
		diags.AddError("API tokens cannot perform this operation",
			fmt.Sprintf("%s%s. Perform this operation in the portal, signed in with an account that has the required organization role.", e.Message, rid))
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		diags.AddError("Aether API rejected the request",
			fmt.Sprintf("%d %s%s. Check api_token (created in the portal) and, if set, organization_id.", e.Status, e.Message, rid))
	case e.Status == http.StatusUnprocessableEntity && len(e.Fields) > 0:
		for _, f := range e.Fields {
			diags.AddAttributeError(fieldPath(attrPrefix, f.Field), "Invalid value",
				fmt.Sprintf("%s (%s)%s", f.Message, f.Code, rid))
		}
	case e.Status == http.StatusConflict && e.Code == CodeLastOwner:
		diags.AddError("Cannot remove the organization's last owner",
			fmt.Sprintf("%s%s. Add another owner first, then remove or replace this one in a later apply.", e.Message, rid))
	case e.Status == http.StatusConflict:
		diags.AddError("Conflict", e.Message+rid)
	default:
		diags.AddError("Aether API error", fmt.Sprintf("%d %s: %s%s", e.Status, e.Code, e.Message, rid))
	}
}

// fieldPath turns a dotted API field name ("node_pools.name") into an attribute
// path under prefix. An empty field name yields the prefix itself, so a 422
// entry without a field still lands somewhere addressable.
func fieldPath(prefix path.Path, field string) path.Path {
	p := prefix
	for _, seg := range strings.Split(field, ".") {
		if seg != "" {
			p = p.AtName(seg)
		}
	}
	return p
}

// Truncate bounds an untrusted string before it goes into a diagnostic, and
// guarantees the result is valid UTF-8 on EVERY path - not only when it cuts.
// A response body is bytes from elsewhere: it can be invalid UTF-8 at any
// offset, including within the first n bytes, and a diagnostic carrying
// invalid UTF-8 fails to serialise over the plugin protocol. Cutting is also
// done on a rune boundary, so a truncation never manufactures a replacement
// character of its own.
//
// Exported because every caller that puts a raw body into a diagnostic needs
// it; an unbounded body in a diagnostic is a defect, not a verbosity choice.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "")
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.ToValidUTF8(cut, "") + "…"
}
