package fakeapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const webhooksPath = "/organizations/" + testOrg + "/webhooks"

func postWebhook(t *testing.T, s *Server, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, b := call(t, s, http.MethodPost, webhooksPath, body)
	return resp, decode(t, b)
}

const validWebhookBody = `{"url":"https://example.com/hook","events":["cluster.ready"]}`

// Fails if POST 201 drops a key, includes an unexpected one, or leaks a
// non-"whsec_"-shaped or wrong-length secret. Pins the create response shape
// (§2) that every other webhook test builds on.
func TestWebhookPostIs201WithSecretAndTheExactKeySet(t *testing.T) {
	s := newClusterFake(t)
	resp, m := postWebhook(t, s, validWebhookBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST = %d %v, want 201", resp.StatusCode, m)
	}
	got := map[string]bool{}
	for k := range m {
		got[k] = true
	}
	for _, k := range []string{"id", "org_id", "url", "description", "events", "enabled", "secret", "created_at", "updated_at"} {
		if !got[k] {
			t.Fatalf("missing %s: %v", k, m)
		}
		delete(got, k)
	}
	if len(got) != 0 {
		t.Fatalf("extra keys %v in %v", got, m)
	}
	secret, _ := m["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") || len(secret) != 54 {
		t.Fatalf("secret = %q, want \"whsec_\" + 48 hex chars (54 total)", secret)
	}
	if m["description"] != "" || m["enabled"] != true || m["org_id"] != testOrg {
		t.Fatalf("body = %v", m)
	}
}

// Fails if `enabled` or `secret` in the POST body is accepted (a webhook can
// only be created enabled; secret is server-generated) instead of the
// measured 400 bad_request (§2).
func TestWebhookPostRejectsEnabledAndSecretWith400(t *testing.T) {
	for _, body := range []string{
		`{"url":"https://example.com/hook","events":["cluster.ready"],"enabled":false}`,
		`{"url":"https://example.com/hook","events":["cluster.ready"],"secret":"whsec_x"}`,
	} {
		s := newClusterFake(t)
		resp, m := postWebhook(t, s, body)
		if resp.StatusCode != http.StatusBadRequest || errCode(m) != "bad_request" {
			t.Fatalf("body %s = %d %v, want 400 bad_request", body, resp.StatusCode, m)
		}
	}
}

// Fails if either half of the "url empty AND events empty" case is missing
// from the details list, or if unknown-event/wildcard/url validation runs
// before both required checks pass (§2 step order).
func TestWebhookPostValidationOrderAndDetails(t *testing.T) {
	s := newClusterFake(t)
	resp, m := postWebhook(t, s, `{"url":"","events":[]}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d %v, want 422", resp.StatusCode, m)
	}
	d := detailsOf(m)
	if len(d) != 2 {
		t.Fatalf("details = %v, want exactly two (url+events required together)", d)
	}
	byField := map[string]map[string]any{}
	for _, e := range d {
		f := e.(map[string]any)
		byField[f["field"].(string)] = f
	}
	if f := byField["url"]; f["code"] != "required" || f["message"] != "URL is required" {
		t.Fatalf("url detail = %v", f)
	}
	if f := byField["events"]; f["code"] != "required" || f["message"] != "At least one event is required" {
		t.Fatalf("events detail = %v", f)
	}

	for _, c := range []struct {
		name, body, field, code, msg string
	}{
		{"unknown event", `{"url":"https://x.io/h","events":["bogus"]}`, "events", "invalid", `unknown event type "bogus"`},
		{"wildcard not alone", `{"url":"https://x.io/h","events":["*","cluster.ready"]}`, "events", "invalid", "wildcard '*' must be the only entry"},
		{"non-https", `{"url":"http://x.io/h","events":["cluster.ready"]}`, "url", "invalid", "url must use https"},
		{"credentials", `{"url":"https://u:p@x.io/h","events":["cluster.ready"]}`, "url", "invalid", "url must not contain credentials"},
		{"blocked ip", `{"url":"https://127.0.0.1/h","events":["cluster.ready"]}`, "url", "invalid", "url host resolves to a blocked address range"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newClusterFake(t)
			resp, m := postWebhook(t, s, c.body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d %v, want 422", resp.StatusCode, m)
			}
			d := detailsOf(m)
			if len(d) != 1 {
				t.Fatalf("details = %v, want exactly one", d)
			}
			f := d[0].(map[string]any)
			if f["field"] != c.field || f["code"] != c.code || f["message"] != c.msg {
				t.Fatalf("detail = %v, want %s/%s/%q", f, c.field, c.code, c.msg)
			}
		})
	}
}

// Fails if hostnames are resolved/refused at create time: the API defers that
// to delivery, so "https://localhost/" must be ACCEPTED (§2, I3).
func TestWebhookPostAcceptsLocalhostURL(t *testing.T) {
	s := newClusterFake(t)
	resp, m := postWebhook(t, s, `{"url":"https://localhost/hook","events":["cluster.ready"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST = %d %v, want 201 (hostnames are not resolved at create time)", resp.StatusCode, m)
	}
}

// Fails if events are de-duplicated, sorted, or otherwise normalised on
// write: the API stores them exactly as sent (§2/§3/I4).
func TestWebhookPostStoresEventsVerbatimNoDedupeNoSort(t *testing.T) {
	s := newClusterFake(t)
	_, m := postWebhook(t, s, `{"url":"https://x.io/h","events":["cluster.ready","cluster.ready","cluster.created"]}`)
	got := m["events"].([]any)
	want := []string{"cluster.ready", "cluster.ready", "cluster.created"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v verbatim", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("events[%d] = %v, want %q (order/duplicates must survive)", i, got[i], w)
		}
	}
}

// Fails if GET, PUT or the list response leak `secret` (only create and
// rotate may include it, §2/§4/§5/§6).
func TestWebhookSecretIsOmittedFromGetPutAndList(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	id := created["id"].(string)

	if resp, b := call(t, s, http.MethodGet, webhooksPath+"/"+id, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d %s", resp.StatusCode, b)
	} else if _, has := decode(t, b)["secret"]; has {
		t.Fatalf("GET leaked secret: %s", b)
	}

	resp, b := call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"description":"x"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d %s", resp.StatusCode, b)
	}
	if _, has := decode(t, b)["secret"]; has {
		t.Fatalf("PUT leaked secret: %s", b)
	}

	_, lb := call(t, s, http.MethodGet, webhooksPath, "")
	items := decode(t, lb)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list = %v, want one item", items)
	}
	if _, has := items[0].(map[string]any)["secret"]; has {
		t.Fatalf("list leaked secret: %s", lb)
	}
}

// Fails if `{}` is refused, or if it does not bump updated_at (§5): the
// COALESCE merge treats an empty object as a no-op WRITE, not a no-op.
func TestWebhookPutEmptyObjectIsValidAndBumpsUpdatedAt(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	id, before := created["id"].(string), created["updated_at"].(string)

	resp, b := call(t, s, http.MethodPut, webhooksPath+"/"+id, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT {} = %d %s, want 200", resp.StatusCode, b)
	}
	after := decode(t, b)["updated_at"].(string)
	if after == before {
		t.Fatalf("updated_at unchanged after PUT {} (%s), want it to move", after)
	}
	if got := decode(t, b)["url"]; got != "https://example.com/hook" {
		t.Fatalf("PUT {} changed url to %v, want it kept", got)
	}
}

// Fails if PUT `events:[]` is accepted, or if its message is create's
// "At least one event is required" instead of the DIFFERENT measured PUT
// wording (§5).
func TestWebhookPutEmptyEventsIs422WithTheMeasuredMessage(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	id := created["id"].(string)
	resp, b := call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"events":[]}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT events:[] = %d %s, want 422", resp.StatusCode, b)
	}
	m := decode(t, b)
	d := detailsOf(m)
	if len(d) != 1 {
		t.Fatalf("details = %v, want exactly one", d)
	}
	f := d[0].(map[string]any)
	if f["field"] != "events" || f["code"] != "invalid" || f["message"] != "at least one event type is required" {
		t.Fatalf("detail = %v, want events/invalid/\"at least one event type is required\"", f)
	}
}

// Fails if description:"" is treated as "omit" (keep) instead of the API's
// only clear, or if an omitted/null description overwrites the stored value.
func TestWebhookPutDescriptionCoalesceSemantics(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, `{"url":"https://x.io/h","description":"first","events":["cluster.ready"]}`)
	id := created["id"].(string)

	// {} keeps the description.
	_, b := call(t, s, http.MethodPut, webhooksPath+"/"+id, `{}`)
	if got := decode(t, b)["description"]; got != "first" {
		t.Fatalf("PUT {} description = %v, want kept as \"first\"", got)
	}
	// An explicit "" clears it.
	_, b = call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"description":""}`)
	if got := decode(t, b)["description"]; got != "" {
		t.Fatalf("PUT description:\"\" = %v, want cleared to \"\"", got)
	}
}

// Fails if rotate does not return a brand-new, correctly shaped secret, if it
// does not bump updated_at, or if it refuses a disabled webhook.
func TestWebhookRotateReturnsNewSecretAndBumpsUpdatedAt(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	id, firstSecret, before := created["id"].(string), created["secret"].(string), created["updated_at"].(string)

	// Disable it first - rotate must still work (§6: no enabled check).
	call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"enabled":false}`)

	resp, b := call(t, s, http.MethodPost, webhooksPath+"/"+id+"/rotate-secret", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate = %d %s, want 200", resp.StatusCode, b)
	}
	m := decode(t, b)
	secret, _ := m["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") || len(secret) != 54 {
		t.Fatalf("rotated secret = %q, want \"whsec_\" + 48 hex chars", secret)
	}
	if secret == firstSecret {
		t.Fatalf("rotate returned the SAME secret as create")
	}
	if m["updated_at"] == before {
		t.Fatalf("updated_at unchanged after rotate, want it to move")
	}
	if m["enabled"] != false {
		t.Fatalf("rotate changed enabled to %v, want it untouched (false)", m["enabled"])
	}
	// A second rotate yields yet another secret: never idempotent (§6).
	_, b2 := call(t, s, http.MethodPost, webhooksPath+"/"+id+"/rotate-secret", "")
	if second := decode(t, b2)["secret"]; second == secret {
		t.Fatalf("second rotate returned the same secret: rotation must never be idempotent")
	}
}

// Fails if the 11th webhook of an org - counting disabled ones - is not 409
// webhook_limit (§2). Every one of the ten is disabled, so a cap that only
// counted enabled webhooks would wrongly allow an 11th.
func TestWebhookElevenththIsWebhookLimitCountingDisabled(t *testing.T) {
	s := newClusterFake(t)
	for i := 0; i < webhookCap; i++ {
		resp, m := postWebhook(t, s, fmt.Sprintf(`{"url":"https://x.io/h%d","events":["cluster.ready"]}`, i))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("webhook %d = %d %v, want 201", i, resp.StatusCode, m)
		}
		id := m["id"].(string)
		if resp, b := call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"enabled":false}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("disabling webhook %d: PUT = %d %s", i, resp.StatusCode, b)
		}
	}
	resp, m := postWebhook(t, s, `{"url":"https://x.io/h-eleventh","events":["cluster.ready"]}`)
	if resp.StatusCode != http.StatusConflict || errCode(m) != "webhook_limit" ||
		m["error"].(map[string]any)["message"] != "Webhook limit reached for this organization" {
		t.Fatalf("11th webhook = %d %v, want 409 webhook_limit \"Webhook limit reached for this organization\"", resp.StatusCode, m)
	}
}

// Fails if DELETE is not 204, or if a second DELETE (or a GET afterwards) is
// anything but 404 - a hard delete, never 410 (§7).
func TestWebhookDeleteIsHardDeleteThenNotFound(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	p := webhooksPath + "/" + created["id"].(string)

	resp, b := call(t, s, http.MethodDelete, p, "")
	if resp.StatusCode != http.StatusNoContent || len(b) != 0 {
		t.Fatalf("DELETE = %d %q, want 204 empty", resp.StatusCode, b)
	}
	if resp, _ := call(t, s, http.MethodDelete, p, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE = %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodGet, p, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE = %d, want 404", resp.StatusCode)
	}
}

// Fails if a webhook id belonging to another org is visible (GET/PUT/DELETE)
// through THIS org's path: it must be 404, never leaked or 403 (§4).
func TestWebhookOfAnotherOrgIs404UnderThisOrgsPath(t *testing.T) {
	s := newClusterFake(t)
	const other = "33333333-3333-4333-8333-333333333333"
	s.AddOrg(other, "Other", "other")
	id := s.SeedWebhook(other, "https://other.io/h", []string{"cluster.ready"}, true)

	if resp, _ := call(t, s, http.MethodGet, webhooksPath+"/"+id, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-org GET = %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodPut, webhooksPath+"/"+id, `{}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-org PUT = %d, want 404", resp.StatusCode)
	}
	if resp, _ := call(t, s, http.MethodDelete, webhooksPath+"/"+id, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-org DELETE = %d, want 404", resp.StatusCode)
	}
	// It is still reachable under its OWN org.
	if resp, _ := call(t, s, http.MethodGet, "/organizations/"+other+"/webhooks/"+id, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET under the owning org = %d, want 200", resp.StatusCode)
	}
}

// Fails if a foreign org (AddForeignOrg: exists on the platform, but this
// token cannot reach it) answers anything but 403 on a webhook route - the
// org-mismatch check must run BEFORE any webhook-specific code (§9), unlike
// the 404 an unknown-id-under-a-known-org gets.
func TestWebhookForeignOrgIs403(t *testing.T) {
	s := newClusterFake(t)
	s.AddForeignOrg(foreignOrg, "Them", "them")
	p := "/organizations/" + foreignOrg + "/webhooks"
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, p, ""},
		{http.MethodPost, p, validWebhookBody},
		{http.MethodGet, p + "/" + foreignOrg, ""},
		{http.MethodPut, p + "/" + foreignOrg, `{}`},
		{http.MethodDelete, p + "/" + foreignOrg, ""},
		{http.MethodPost, p + "/" + foreignOrg + "/rotate-secret", ""},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			resp, b := call(t, s, c.method, c.path, c.body)
			m := decode(t, b)
			if resp.StatusCode != http.StatusForbidden || errCode(m) != "forbidden" {
				t.Fatalf("%s %s = %d %s, want 403 forbidden", c.method, c.path, resp.StatusCode, b)
			}
		})
	}
}

// Fails if FailNextWebhookPUT does not intercept the very next PUT (whatever
// id it targets), does not restore normal routing afterwards, or leaks across
// organizations.
func TestFailNextWebhookPUTIsOneShotAndPerOrg(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	id := created["id"].(string)
	s.FailNextWebhookPUT(testOrg, http.StatusInternalServerError, "internal_error", "boom")

	resp, b := call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"description":"x"}`)
	if resp.StatusCode != http.StatusInternalServerError || errCode(decode(t, b)) != "internal_error" {
		t.Fatalf("armed PUT = %d %s, want 500 internal_error", resp.StatusCode, b)
	}
	// Consumed: the next PUT goes through normally.
	resp, b = call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"description":"y"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second PUT = %d %s, want 200 (the override is one-shot)", resp.StatusCode, b)
	}
	if got := decode(t, b)["description"]; got != "y" {
		t.Fatalf("description = %v, want \"y\" (a real write happened)", got)
	}
}

// Fails if a PUT to an unknown id consumes an armed FailNextWebhookPUT
// override before the id lookup runs - it must 404 (the id lookup precedes
// the failure check) AND leave the override armed for the next PUT, since a
// real webhook's id is never known to the override (CodeRabbit PR #15 CR-2).
func TestFailNextWebhookPUTUnknownIDDoesNotConsumeOverride(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	id := created["id"].(string)
	s.FailNextWebhookPUT(testOrg, http.StatusInternalServerError, "internal_error", "boom")

	resp, b := call(t, s, http.MethodPut, webhooksPath+"/nonexistent-id", `{"description":"x"}`)
	if resp.StatusCode != http.StatusNotFound || errCode(decode(t, b)) != "not_found" {
		t.Fatalf("PUT unknown id with an armed override = %d %s, want 404 not_found", resp.StatusCode, b)
	}

	// The override must still be armed - it targets the real webhook now.
	resp, b = call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"description":"y"}`)
	if resp.StatusCode != http.StatusInternalServerError || errCode(decode(t, b)) != "internal_error" {
		t.Fatalf("PUT to the real id after the 404 = %d %s, want 500 internal_error (override should still be armed)", resp.StatusCode, b)
	}
}

// Rotate's sibling of TestFailNextWebhookPUTUnknownIDDoesNotConsumeOverride.
func TestFailNextWebhookRotateUnknownIDDoesNotConsumeOverride(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	id := created["id"].(string)
	s.FailNextWebhookRotate(testOrg, http.StatusInternalServerError, "internal_error", "boom")

	resp, b := call(t, s, http.MethodPost, webhooksPath+"/nonexistent-id/rotate-secret", "")
	if resp.StatusCode != http.StatusNotFound || errCode(decode(t, b)) != "not_found" {
		t.Fatalf("rotate unknown id with an armed override = %d %s, want 404 not_found", resp.StatusCode, b)
	}

	resp, b = call(t, s, http.MethodPost, webhooksPath+"/"+id+"/rotate-secret", "")
	if resp.StatusCode != http.StatusInternalServerError || errCode(decode(t, b)) != "internal_error" {
		t.Fatalf("rotate the real id after the 404 = %d %s, want 500 internal_error (override should still be armed)", resp.StatusCode, b)
	}
}

// Fails if FailNextWebhookRotate does not intercept rotate specifically
// (leaving PUT untouched), or is not one-shot.
func TestFailNextWebhookRotateIsOneShot(t *testing.T) {
	s := newClusterFake(t)
	_, created := postWebhook(t, s, validWebhookBody)
	id := created["id"].(string)
	s.FailNextWebhookRotate(testOrg, http.StatusInternalServerError, "internal_error", "boom")

	// An ordinary PUT must NOT be affected by the rotate-only override.
	if resp, b := call(t, s, http.MethodPut, webhooksPath+"/"+id, `{"description":"x"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT while a rotate failure is armed = %d %s, want 200", resp.StatusCode, b)
	}

	resp, b := call(t, s, http.MethodPost, webhooksPath+"/"+id+"/rotate-secret", "")
	if resp.StatusCode != http.StatusInternalServerError || errCode(decode(t, b)) != "internal_error" {
		t.Fatalf("armed rotate = %d %s, want 500 internal_error", resp.StatusCode, b)
	}
	resp, b = call(t, s, http.MethodPost, webhooksPath+"/"+id+"/rotate-secret", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second rotate = %d %s, want 200 (the override is one-shot)", resp.StatusCode, b)
	}
}

// Fails if the list clamps limit incorrectly, omits next_cursor on a full
// page, a webhook appears on two pages, or a full-then-partial page sequence
// doesn't terminate with an unset next_cursor (§10 - minimal coverage, since
// the provider itself never calls this route).
func TestWebhookListPaginatesByID(t *testing.T) {
	s := newClusterFake(t)
	for i := 0; i < 3; i++ {
		s.SeedWebhook(testOrg, fmt.Sprintf("https://x.io/h%d", i), []string{"cluster.ready"}, true)
	}
	_, b := call(t, s, http.MethodGet, webhooksPath+"?limit=2", "")
	m := decode(t, b)
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("page 1 = %d items, want 2", len(items))
	}
	cursor, has := m["next_cursor"]
	if !has {
		t.Fatalf("full page lacks next_cursor: %s", b)
	}
	page1IDs := map[string]bool{}
	for _, it := range items {
		page1IDs[it.(map[string]any)["id"].(string)] = true
	}

	_, b2 := call(t, s, http.MethodGet, webhooksPath+"?limit=2&cursor="+cursor.(string), "")
	m2 := decode(t, b2)
	items2 := m2["items"].([]any)
	if len(items2) != 1 {
		t.Fatalf("page 2 = %d items, want 1", len(items2))
	}
	id2 := items2[0].(map[string]any)["id"].(string)
	if page1IDs[id2] {
		t.Fatalf("page 2 item %s already appeared on page 1 %v", id2, page1IDs)
	}
	if _, has := m2["next_cursor"]; has {
		t.Fatalf("partial page carries next_cursor: %s", b2)
	}
}
