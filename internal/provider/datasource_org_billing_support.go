package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/plane-shift/terraform-provider-aether/internal/client"
)

// Helpers shared by the Task 14 organization / registry / billing data
// sources.

// readRawBody reads and closes a PLAIN (non-WithResponse) client response.
// Raw-decoded routes never use the *WithResponse form (T14-17): it first
// json-decodes the whole body into the generated types, so a value the
// provider never exposes (an unknown enum, a non-UUID id, an absent
// timestamp) could fail the read before the provider's own decode ran.
// A read-side Close error cannot change what was read; it is discarded.
func readRawBody(hr *http.Response) ([]byte, error) {
	defer func() { _ = hr.Body.Close() }()
	return io.ReadAll(hr.Body)
}

// pageDiagsError carries diagnostics (an apierr-mapped API error, a decode
// failure) through collectAll's plain error return, so a paged read reports
// exactly what a single read would.
type pageDiagsError struct{ diags diag.Diagnostics }

func (e *pageDiagsError) Error() string {
	for _, d := range e.diags {
		if d.Severity() == diag.SeverityError {
			return d.Summary() + ": " + d.Detail()
		}
	}
	return "paged read failed"
}

// pageFailure wraps diags for collectAll; nil when diags holds no error.
func pageFailure(diags diag.Diagnostics) error {
	if !diags.HasError() {
		return nil
	}
	return &pageDiagsError{diags: diags}
}

// addPageError appends a collectAll error: the carried diagnostics when it is
// a pageDiagsError, otherwise one error under summary (a transport failure).
func addPageError(diags *diag.Diagnostics, summary string, err error) {
	var pe *pageDiagsError
	if errors.As(err, &pe) {
		diags.Append(pe.diags...)
		return
	}
	diags.AddError(summary, err.Error())
}

// addNoJSONBodyError is the reference data source's error for a 200 whose
// body the generated client could not decode (JSON200 == nil).
func addNoJSONBodyError(diags *diag.Diagnostics, hr *http.Response, status string) {
	diags.AddError("Aether API answered 200 without a JSON body",
		fmt.Sprintf("GET %s returned %s with content type %q.", hr.Request.URL, status, hr.Header.Get("Content-Type")))
}

// optTimeRFC3339 maps an optional API time onto a Terraform value: absent →
// null, never a zero time rendered as 0001-01-01.
func optTimeRFC3339(t *time.Time) types.String {
	if t == nil {
		return types.StringNull()
	}
	return types.StringValue(t.Format(time.RFC3339))
}

// optUUIDString maps an optional API UUID onto canonical text or null.
func optUUIDString(u *openapi_types.UUID) types.String {
	if u == nil {
		return types.StringNull()
	}
	return types.StringValue(u.String())
}

// nextUUIDCursor validates a next_cursor from a UUID-cursor list and returns
// it as canonical text ("" when the list ended), the form collectAll compares
// and uuidCursorParam parses back.
func nextUUIDCursor(next *string) (string, error) {
	if next == nil || *next == "" {
		return "", nil
	}
	c, err := parseID(*next)
	if err != nil {
		return "", fmt.Errorf("the API returned an unusable next_cursor: %w", err)
	}
	return c.String(), nil
}

// uuidCursorParam is the typed cursor for a request: nil on the first page.
func uuidCursorParam(cursor string) (*client.Cursor, error) {
	if cursor == "" {
		return nil, nil
	}
	c, err := parseID(cursor)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// harborPageEditor adds the Harbor page-number cursor and the page size to a
// registry list request: the generated repositories/tags calls take no
// parameters because the OpenAPI document declares none (contract §1/§2).
func harborPageEditor(cursor string) client.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		q := req.URL.Query()
		q.Set("limit", "100")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req.URL.RawQuery = q.Encode()
		return nil
	}
}
