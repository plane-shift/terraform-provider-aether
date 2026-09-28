package provider_test

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

// These tests drive aether_webhook (internal/provider/resource_webhook.go,
// registered in provider.go's Resources() as NewWebhookResource) through a
// real terraform (or tofu) binary against the in-process fake
// (fakeapi/webhooks.go), under the controller rulings T10-0..T10-14 of
// task-10-brief-amended.md. Tests 1-18 are the amended brief's numbered
// list (see the numbered section markers below); the two after them fix
// review r1 findings I-1 and M-2 - see task-10-tests-report.md for the
// per-test rationale and the review fix-round sections for what each one
// added and why.

const (
	webhookResource = "aether_webhook.w"
	webhooksListRE  = `/webhooks$`
	webhookOneRE    = `/webhooks/[^/]+$`
	webhookRotateRE = `/webhooks/[^/]+/rotate-secret$`
)

// webhookSecretRE is the §2/§6 secret shape: "whsec_" + 48 hex chars (54
// total).
var webhookSecretRE = regexp.MustCompile(`^whsec_[0-9a-f]{48}$`)

// webhookHCL renders one aether_webhook resource. events is always rendered
// as an explicit HCL list literal (events is Required, so every config sets
// it - an empty slice renders `events = []`, which the SizeAtLeast(1)
// validator must refuse at plan time, never an omitted attribute). extra
// lines (description, enabled, rotate_secret, ...) go inside the block.
func webhookHCL(url string, events []string, extra ...string) string {
	quoted := make([]string, len(events))
	for i, e := range events {
		quoted[i] = fmt.Sprintf("%q", e)
	}
	return fmt.Sprintf(`
provider "aether" {}
resource "aether_webhook" "w" {
  url    = %q
  events = [%s]
%s
}
`, url, strings.Join(quoted, ", "), strings.Join(extra, "\n"))
}

// webhookWrites returns "POST"/"PUT"/"ROTATE"/"DELETE" for every webhook
// write since the n-th recorded request, in order - the shape every
// PUT-then-rotate / rotate-only / PUT-only ordering test in this file checks
// against.
func webhookWrites(api *fakeapi.Server, n int) []string {
	rotateRE, oneRE := regexp.MustCompile(webhookRotateRE), regexp.MustCompile(webhookOneRE)
	listRE := regexp.MustCompile(webhooksListRE)
	var out []string
	for _, r := range requestsSince(api, n) {
		switch {
		case r.Method == http.MethodPost && rotateRE.MatchString(r.Path):
			out = append(out, "ROTATE")
		case r.Method == http.MethodPost && listRE.MatchString(r.Path):
			out = append(out, "POST")
		case r.Method == http.MethodPut && oneRE.MatchString(r.Path):
			out = append(out, "PUT")
		case r.Method == http.MethodDelete && oneRE.MatchString(r.Path):
			out = append(out, "DELETE")
		}
	}
	return out
}

// webhookPUTsSince is webhookWrites narrowed to PUT requests only, for tests
// that need the exact body of a single PUT.
func webhookPUTsSince(api *fakeapi.Server, n int) []fakeapi.Request {
	re := regexp.MustCompile(webhookOneRE)
	var out []fakeapi.Request
	for _, r := range requestsSince(api, n) {
		if r.Method == http.MethodPut && re.MatchString(r.Path) {
			out = append(out, r)
		}
	}
	return out
}

// attrs is a shorthand for a resource's flat state attribute map.
func attrs(s *terraform.State, addr string) map[string]string {
	rs := s.RootModule().Resources[addr]
	if rs == nil {
		return nil
	}
	return rs.Primary.Attributes
}

// ---------------------------------------------------------------- 1

// Fails if Create sends anything but {url, description:"", events:[sorted]}
// (an extra enabled/secret key, unsorted events), if it ever PUTs on a plain
// create, or if enabled/description/rotate_secret/secret/timestamps are not
// exactly what T10-3/T10-4/T10-1/T10-7 say.
func TestWebhookResource_CreateMinimalSendsExactBodyAndState(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			// Unsorted on purpose: cluster.created < cluster.ready alphabetically,
			// so a POST that echoes config order instead of sorting is caught.
			Config: webhookHCL("https://example.com/hook", []string{"cluster.ready", "cluster.created"}),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(webhookResource, "url", "https://example.com/hook"),
				resource.TestCheckResourceAttr(webhookResource, "enabled", "true"),
				resource.TestCheckResourceAttr(webhookResource, "description", ""),
				resource.TestCheckResourceAttr(webhookResource, "events.#", "2"),
				resource.TestCheckTypeSetElemAttr(webhookResource, "events.*", "cluster.ready"),
				resource.TestCheckTypeSetElemAttr(webhookResource, "events.*", "cluster.created"),
				resource.TestCheckResourceAttrSet(webhookResource, "id"),
				resource.TestCheckResourceAttrSet(webhookResource, "created_at"),
				resource.TestCheckResourceAttrSet(webhookResource, "updated_at"),
				resource.TestCheckNoResourceAttr(webhookResource, "rotate_secret"),
				resource.TestMatchResourceAttr(webhookResource, "secret", webhookSecretRE),
				func(*terraform.State) error {
					posts := requestsMatching(api, http.MethodPost, webhooksListRE)
					if len(posts) != 1 {
						return fmt.Errorf("want exactly one POST, got %d: %v", len(posts), posts)
					}
					if got, want := compactJSON(t, posts[0].Body), `{"description":"","events":["cluster.created","cluster.ready"],"url":"https://example.com/hook"}`; got != want {
						return fmt.Errorf("POST body = %s, want exactly %s", got, want)
					}
					if p := requestsMatching(api, http.MethodPut, webhookOneRE); len(p) != 0 {
						return fmt.Errorf("a PUT was sent for a plain create: %v", p)
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- 2

// Fails if enabled=false at create either omits the disabling PUT (leaving
// the webhook enabled) or folds `enabled` into the POST body (which the fake
// - and the real API - refuses with 400): T10-3 requires POST (created
// enabled) THEN PUT {"enabled":false}, in that order, with no other keys.
func TestWebhookResource_CreateDisabledPostsThenPutsExactBody(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: webhookHCL("https://example.com/hook", []string{"cluster.ready"}, "enabled = false"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(webhookResource, "enabled", "false"),
				resource.TestMatchResourceAttr(webhookResource, "secret", webhookSecretRE),
				func(*terraform.State) error {
					posts := requestsMatching(api, http.MethodPost, webhooksListRE)
					if len(posts) != 1 {
						return fmt.Errorf("want exactly one POST, got %d", len(posts))
					}
					if strings.Contains(string(posts[0].Body), "enabled") {
						return fmt.Errorf("POST body carries enabled, which the API 400s on: %s", posts[0].Body)
					}
					puts := webhookPUTsSince(api, 0)
					if len(puts) != 1 {
						return fmt.Errorf("want exactly one PUT, got %d: %v", len(puts), puts)
					}
					if got, want := compactJSON(t, puts[0].Body), `{"enabled":false}`; got != want {
						return fmt.Errorf("PUT body = %s, want exactly %s", got, want)
					}
					if w := webhookWrites(api, 0); strings.Join(w, ",") != "POST,PUT" {
						return fmt.Errorf("writes = %v, want POST then PUT in that order", w)
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- 3

// Fails if a failed disabling PUT is rolled back with a DELETE (T10-3
// forbids "delete-and-fail": the secret would be lost with no way to import
// the orphaned webhook back), if the created webhook's id is not saved into
// state (proven twice: directly, by a RefreshState step reading the SAVED
// state right after the error - review r1 M-1 - and again by the id
// surviving into the NEXT apply's replace, which needs a real id to send a
// DELETE for), or if Terraform does not taint the resource so the next apply
// replaces it.
func TestWebhookResource_CreateDisabledWhenThePUTFailsSavesStateAndTaints(t *testing.T) {
	api := setupFake(t)
	// T10-12: NOT a 5xx. PUT is idempotent (internal/provider/transport/retry.go
	// isIdempotent), so a 500 here is retried by the provider's own transport -
	// the one-shot override is consumed by the first attempt, the retry gets a
	// normal 200, and no error ever reaches the resource. 422 is never retried.
	api.FailNextWebhookPUT(testOrg, http.StatusUnprocessableEntity, "validation_error", "disable failed")
	cfg := webhookHCL("https://example.com/hook", []string{"cluster.ready"}, "enabled = false")
	var oldID string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: cfg, ExpectError: loose("disable failed")},
			{
				// M-1: RefreshState just re-reads the SAVED state (Read, no PUT/
				// rotate) - the only way to see what the errored Create actually
				// wrote, before the recovering apply below can overwrite it. The
				// config still asks for enabled=false while the real (and saved)
				// state is enabled=true, so the post-refresh plan is non-empty.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					a := attrs(s, webhookResource)
					if a["enabled"] != "true" {
						return fmt.Errorf("saved enabled = %q, want %q (the 201's value, T10-3 - the disable never applied)", a["enabled"], "true")
					}
					if !webhookSecretRE.MatchString(a["secret"]) {
						return fmt.Errorf("saved secret = %q, want the 201's whsec_-shaped value", a["secret"])
					}
					return nil
				},
			},
			{
				PreConfig: func() {
					if d := requestsMatching(api, http.MethodDelete, webhookOneRE); len(d) != 0 {
						t.Fatalf("a DELETE was sent after the failed disable - never roll back a create by deleting: %v", d)
					}
					ids := api.WebhookIDs(testOrg)
					if len(ids) != 1 {
						t.Fatalf("webhooks in the fake = %v, want exactly the one the failed create's POST made (state must have kept it, not orphaned it)", ids)
					}
					oldID = ids[0]
				},
				Config: cfg,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(webhookResource, "enabled", "false"),
					func(s *terraform.State) error {
						deletes := requestsMatching(api, http.MethodDelete, webhookOneRE)
						if len(deletes) != 1 || !strings.HasSuffix(deletes[0].Path, "/"+oldID) {
							return fmt.Errorf("the recovering apply's DELETEs = %v, want exactly one targeting the failed create's id %s - proof the tainted state kept that id", deletes, oldID)
						}
						newID := attrs(s, webhookResource)["id"]
						if newID == "" || newID == oldID {
							return fmt.Errorf("id after recovery = %q (was %q), want a NEW id from the replacement's POST", newID, oldID)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 4

// Fails if setting rotate_secret at CREATE time also calls rotate: create
// already returns a fresh secret, so a rotate here would be an extra,
// unnecessary write (T10-2).
func TestWebhookResource_CreateWithRotateSecretSetCallsNoRotate(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: webhookHCL("https://example.com/hook", []string{"cluster.ready"}, `rotate_secret = "1"`),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(webhookResource, "rotate_secret", "1"),
				func(*terraform.State) error {
					if r := requestsMatching(api, http.MethodPost, webhookRotateRE); len(r) != 0 {
						return fmt.Errorf("create with rotate_secret set called rotate %d time(s), want zero", len(r))
					}
					return nil
				},
			),
		}},
	})
}

// ---------------------------------------------------------------- 5

// Fails if changing rotate_secret sends a PUT (it must not - no managed
// field changed), if it fails to rotate at all, or if the new secret/
// updated_at do not reach state (which would also surface as Terraform's
// "provider produced inconsistent result after apply", failing the step even
// without an explicit assertion for it). Review r1 I-1: the PreApply plan
// check pins that `secret` is really planned UNKNOWN here (the positive half
// of the rotate trigger), not merely that it ends up different after apply.
func TestWebhookResource_ChangingRotateSecretRotatesOnlyNoPUT(t *testing.T) {
	api := setupFake(t)
	var firstSecret, firstUpdated string
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: webhookHCL("https://example.com/hook", []string{"cluster.ready"}, `rotate_secret = "1"`),
				Check: func(s *terraform.State) error {
					a := attrs(s, webhookResource)
					firstSecret, firstUpdated = a["secret"], a["updated_at"]
					return nil
				},
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://example.com/hook", []string{"cluster.ready"}, `rotate_secret = "2"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectUnknownValue(webhookResource, tfjsonpath.New("secret")),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestMatchResourceAttr(webhookResource, "secret", webhookSecretRE),
					func(s *terraform.State) error {
						if w := webhookWrites(api, seen); strings.Join(w, ",") != "ROTATE" {
							return fmt.Errorf("writes = %v, want exactly one ROTATE and no PUT", w)
						}
						a := attrs(s, webhookResource)
						if a["secret"] == firstSecret {
							return fmt.Errorf("secret unchanged after rotation")
						}
						if a["updated_at"] == firstUpdated {
							return fmt.Errorf("updated_at unchanged after rotation")
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 6

// Fails if removing rotate_secret from config (a value the resource already
// carried) issues any API call: T10-2 says it "rotates nothing", and no
// managed field changed either.
func TestWebhookResource_RemovingRotateSecretFromConfigIsANoop(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: webhookHCL("https://example.com/hook", []string{"cluster.ready"}, `rotate_secret = "1"`)},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://example.com/hook", []string{"cluster.ready"}), // rotate_secret omitted -> null
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckNoResourceAttr(webhookResource, "rotate_secret"),
					func(*terraform.State) error {
						if w := webhookWrites(api, seen); len(w) != 0 {
							return fmt.Errorf("removing rotate_secret wrote: %v, want zero API calls", w)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 7

// Fails if a description-only change sends anything but {"description":"…"},
// or if REMOVING description sends anything but {"description":""} - the
// API's only clear (T10-4/T10-8). Review r1 I-1: the PreApply plan check on
// the second step pins the "only then" half directly - an update that does
// NOT trigger a rotation must plan `secret` as KNOWN (the prior value, not
// unknown) while `updated_at` (which moves on every PUT) IS unknown.
func TestWebhookResource_DescriptionOnlyChangeSendsExactPUT(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: webhookHCL("https://example.com/hook", []string{"cluster.ready"}, `description = "first"`)},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://example.com/hook", []string{"cluster.ready"}, `description = "second"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectKnownValue(webhookResource, tfjsonpath.New("secret"), knownvalue.NotNull()),
					plancheck.ExpectUnknownValue(webhookResource, tfjsonpath.New("updated_at")),
				}},
				Check: func(*terraform.State) error {
					puts := webhookPUTsSince(api, seen)
					if len(puts) != 1 {
						return fmt.Errorf("want exactly one PUT, got %d: %v", len(puts), puts)
					}
					if got, want := compactJSON(t, puts[0].Body), `{"description":"second"}`; got != want {
						return fmt.Errorf("PUT body = %s, want exactly %s", got, want)
					}
					return nil
				},
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://example.com/hook", []string{"cluster.ready"}), // description removed -> "" default (T10-4)
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(webhookResource, "description", ""),
					func(*terraform.State) error {
						puts := webhookPUTsSince(api, seen)
						if len(puts) != 1 {
							return fmt.Errorf("want exactly one PUT, got %d: %v", len(puts), puts)
						}
						if got, want := compactJSON(t, puts[0].Body), `{"description":""}`; got != want {
							return fmt.Errorf("PUT body = %s, want exactly %s (the API's only clear)", got, want)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 8

// Fails if an events-only change sends anything but {"events":[sorted]}
// (extra keys, or config order instead of sorted order).
func TestWebhookResource_EventsOnlyChangeSendsExactSortedPUT(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: webhookHCL("https://example.com/hook", []string{"cluster.ready"})},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://example.com/hook", []string{"node.replaced", "cluster.created"}),
				Check: func(*terraform.State) error {
					puts := webhookPUTsSince(api, seen)
					if len(puts) != 1 {
						return fmt.Errorf("want exactly one PUT, got %d: %v", len(puts), puts)
					}
					if got, want := compactJSON(t, puts[0].Body), `{"events":["cluster.created","node.replaced"]}`; got != want {
						return fmt.Errorf("PUT body = %s, want exactly %s", got, want)
					}
					return nil
				},
			},
		},
	})
}

// ---------------------------------------------------------------- 9

// Fails if changing url and rotate_secret together in one apply skips
// either call, or sends rotate before the PUT (T10-8: "PUT first ... then
// POST /rotate-secret when the trigger fired").
func TestWebhookResource_URLChangeAndRotateInOneApplyPUTsThenRotates(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: webhookHCL("https://a.example.com/hook", []string{"cluster.ready"}, `rotate_secret = "1"`)},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://b.example.com/hook", []string{"cluster.ready"}, `rotate_secret = "2"`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(webhookResource, "url", "https://b.example.com/hook"),
					func(*terraform.State) error {
						if w := webhookWrites(api, seen); strings.Join(w, ",") != "PUT,ROTATE" {
							return fmt.Errorf("writes = %v, want PUT then ROTATE in that order", w)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 10

// Fails if a failed rotate (after a successful PUT) re-sends the PUT (the
// url change is already applied and saved), loses the url change, or fails
// to re-trigger the rotation on the next apply (T10-8: rotate_secret reverts
// to the PRIOR state value on a failed rotate, so the diff against the still
// -newer config value re-fires). A RefreshState step (review r1 M-1) reads
// the SAVED state directly right after the error, so an implementation that
// nulled `secret` on this path (the recovery rotate would silently overwrite
// that) is caught here rather than only downstream.
func TestWebhookResource_UpdatePUTSucceedsRotateFailsKeepsPUTFieldsAndRetriesNextApply(t *testing.T) {
	api := setupFake(t)
	var firstSecret string
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: webhookHCL("https://a.example.com/hook", []string{"cluster.ready"}, `rotate_secret = "1"`),
				Check: func(s *terraform.State) error {
					firstSecret = attrs(s, webhookResource)["secret"]
					return nil
				},
			},
			{
				PreConfig: func() {
					api.FailNextWebhookRotate(testOrg, http.StatusInternalServerError, "internal_error", "rotate boom")
				},
				Config:      webhookHCL("https://b.example.com/hook", []string{"cluster.ready"}, `rotate_secret = "2"`),
				ExpectError: loose("rotate boom"),
			},
			{
				// M-1: rotate_secret in config ("2") still differs from the
				// reverted saved value ("1"), so the post-refresh plan is
				// non-empty (the rotate trigger fires again).
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: func(s *terraform.State) error {
					a := attrs(s, webhookResource)
					if a["url"] != "https://b.example.com/hook" {
						return fmt.Errorf("saved url = %q, want the PUT's value (T10-8: already applied and saved before the rotate ran)", a["url"])
					}
					if a["rotate_secret"] != "1" {
						return fmt.Errorf("saved rotate_secret = %q, want the PRIOR value %q (a failed rotate must revert it, so the next plan re-triggers)", a["rotate_secret"], "1")
					}
					if a["secret"] != firstSecret {
						return fmt.Errorf("saved secret = %q, want unchanged from before the failed rotate (%q, T10-8)", a["secret"], firstSecret)
					}
					return nil
				},
			},
			{
				// Recovery: same config, nothing armed. If state had kept url=b
				// from the successful PUT, this apply needs zero further PUTs; if
				// rotate_secret had reverted to "1" as ruled, the diff against the
				// still-"2" config re-fires the rotate.
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://b.example.com/hook", []string{"cluster.ready"}, `rotate_secret = "2"`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(webhookResource, "url", "https://b.example.com/hook"),
					func(s *terraform.State) error {
						if w := webhookWrites(api, seen); strings.Join(w, ",") != "ROTATE" {
							return fmt.Errorf("recovery apply did %v, want exactly one ROTATE and zero PUT (url must already be saved from the failed attempt's successful PUT)", w)
						}
						if got := attrs(s, webhookResource)["secret"]; got == firstSecret {
							return fmt.Errorf("secret never changed across the whole test: the recovery rotate did not take effect")
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 11

// Fails if any plan-time validator is missing or too permissive: each case
// must be refused WITHOUT a single request reaching the webhooks API.
func TestWebhookResource_PlanTimeValidationRefusesWithZeroWebhookRequests(t *testing.T) {
	cases := []struct {
		name   string
		url    string
		events []string
		expect *regexp.Regexp
	}{
		{"empty events", "https://x.io/h", []string{}, regexp.MustCompile(`(?is)events`)},
		{"wildcard not alone", "https://x.io/h", []string{"*", "cluster.ready"}, loose(`the wildcard "*" must be the only entry`)},
		{"non-https/http scheme", "ftp://x.io/h", []string{"cluster.ready"}, regexp.MustCompile(`(?is)https?`)},
		{"credentials", "https://u:p@x.io/h", []string{"cluster.ready"}, regexp.MustCompile(`(?is)credentials`)},
		{"no path host (empty authority)", "https:///path", []string{"cluster.ready"}, regexp.MustCompile(`(?is)host`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := setupFake(t)
			resource.UnitTest(t, resource.TestCase{
				IsUnitTest:               true,
				ProtoV6ProviderFactories: factories(),
				Steps:                    []resource.TestStep{{Config: webhookHCL(c.url, c.events), ExpectError: c.expect}},
			})
			for _, r := range api.Requests() {
				if strings.Contains(r.Path, "/webhooks") {
					t.Fatalf("%s: a webhooks API call was made: %s %s", c.name, r.Method, r.Path)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- 12

// Fails if the plan-time https-only validator is stricter than the API
// (T10-6: dev may run WEBHOOK_ALLOW_HTTP=true, so the provider must not
// refuse http:// at plan time and let the API decide).
func TestWebhookResource_HTTPSchemePassesPlanTimeValidation(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:             webhookHCL("http://example.com/hook", []string{"cluster.ready"}),
			PlanOnly:           true,
			ExpectNonEmptyPlan: true, // a create is planned - the point is that planning SUCCEEDS at all
		}},
	})
}

// ---------------------------------------------------------------- 13

// Fails if an unknown event type is refused at PLAN time (T10-5 forbids
// hard-coding the catalog) instead of reaching the API and surfacing its
// 422 message verbatim.
func TestWebhookResource_UnknownEventTypeReachesAPIAnd422s(t *testing.T) {
	api := setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      webhookHCL("https://x.io/h", []string{"bogus.event"}),
			ExpectError: loose(`unknown event type "bogus.event"`),
		}},
	})
	if p := requestsMatching(api, http.MethodPost, webhooksListRE); len(p) != 1 {
		t.Fatalf("want exactly one POST reaching the API, got %d: %v", len(p), p)
	}
}

// ---------------------------------------------------------------- 14

// Fails if the 11th webhook's 409 webhook_limit is swallowed or reworded
// instead of surfaced through apierr.
func TestWebhookResource_EleventhCreateSurfaces409WebhookLimit(t *testing.T) {
	api := setupFake(t)
	for i := 0; i < 10; i++ {
		api.SeedWebhook(testOrg, fmt.Sprintf("https://x.io/h%d", i), []string{"cluster.ready"}, true)
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      webhookHCL("https://x.io/eleventh", []string{"cluster.ready"}),
			ExpectError: loose("Webhook limit reached for this organization"),
		}},
	})
}

// ---------------------------------------------------------------- 15

// Fails if Read errors (or panics building a types.Set) on an out-of-band
// duplicate, if it fails to collapse the duplicate to one element, or if a
// plain refresh overwrites secret with anything but the prior state value
// (T10-1: the API never returns secret on GET).
func TestWebhookResource_ReadDeduplicatesEventsAndPreservesSecretAcrossRefresh(t *testing.T) {
	api := setupFake(t)
	var firstSecret string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: webhookHCL("https://x.io/h", []string{"cluster.ready"}),
				Check: func(s *terraform.State) error {
					a := attrs(s, webhookResource)
					firstSecret = a["secret"]
					if !api.SetWebhookEvents(testOrg, a["id"], []string{"cluster.ready", "cluster.ready"}) {
						return fmt.Errorf("webhook %s missing from the fake", a["id"])
					}
					return nil
				},
			},
			{
				Config: webhookHCL("https://x.io/h", []string{"cluster.ready"}),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(webhookResource, "events.#", "1"),
					func(s *terraform.State) error {
						if got := attrs(s, webhookResource)["secret"]; got != firstSecret {
							return fmt.Errorf("secret changed across a plain refresh: got %q, want %q (Read must carry it from prior state)", got, firstSecret)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 16

// Fails if import does not adopt every GET-derived field, if it invents a
// secret (the API never returns one), or if a post-import rotate_secret
// does not obtain exactly one real secret via exactly one rotate call
// (T10-1/T10-2/T10-9).
//
// T10-13: the webhook is SEEDED out of band, never created through a prior
// apply step, and the import uses ImportStateCheck rather than
// ImportStateVerify - mirroring resource_cluster_network_test.go's
// TestClusterNetworkResource_ImportOfANeverConfiguredClusterReadsEmptyCIDRsAndNullRateLimit
// (the same "adopt something this run never created" shape). A prior create
// step is impossible here: with ImportStatePersist true, `terraform import`
// runs in the SAME working directory/state as a preceding Config step, and
// importing an address terraform already manages fails "Resource already
// managed by Terraform" (the D2 defect this replaces). ImportStateVerify
// itself cannot be used for the same reason from the other direction:
// terraform-plugin-testing@v1.16.0's testStepNewImportState reads the
// working directory's state from BEFORE this step to build its "old
// resources" comparison set (testing_new_import_state.go:51-60), and with no
// prior step that set is empty, so ImportStateVerify's own lookup would
// t.Fatalf("... resource with ID ... not found") before ever reaching my
// assertions - failing on a framework precondition, not on the provider.
func TestWebhookResource_ImportByIDThenRotateObtainsASecret(t *testing.T) {
	api := setupFake(t)
	id := api.SeedWebhook(testOrg, "https://x.io/h", []string{"cluster.ready"}, true)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				ResourceName:       webhookResource,
				ImportState:        true,
				ImportStatePersist: true,
				Config:             webhookHCL("https://x.io/h", []string{"cluster.ready"}),
				ImportStateId:      id,
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					if len(is) != 1 {
						return fmt.Errorf("want exactly one imported instance, got %d", len(is))
					}
					a := is[0].Attributes
					for k, want := range map[string]string{
						"id": id, "url": "https://x.io/h", "enabled": "true",
						"description": "", "events.#": "1",
					} {
						if a[k] != want {
							return fmt.Errorf("imported %s = %q, want %q", k, a[k], want)
						}
					}
					for _, k := range []string{"secret", "rotate_secret"} {
						if v, has := a[k]; has {
							return fmt.Errorf("imported %s = %q, want null (the API never returns secret on GET)", k, v)
						}
					}
					if reqs := requestsMatching(api, http.MethodPut, webhookOneRE); len(reqs) != 0 {
						return fmt.Errorf("import must never PUT (adopt-only), got %d: %v", len(reqs), reqs)
					}
					if reqs := requestsMatching(api, http.MethodPost, webhookRotateRE); len(reqs) != 0 {
						return fmt.Errorf("import must never rotate, got %d: %v", len(reqs), reqs)
					}
					return nil
				},
			},
			{
				// Review r1 I-1, negative half after an import: an update that
				// does NOT trigger a rotation (description-only) must plan
				// `secret` as KNOWN NULL - the post-import value - never unknown.
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://x.io/h", []string{"cluster.ready"}, `description = "d2"`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectKnownValue(webhookResource, tfjsonpath.New("secret"), knownvalue.Null()),
				}},
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(webhookResource, "description", "d2"),
					func(*terraform.State) error {
						if w := webhookWrites(api, seen); strings.Join(w, ",") != "PUT" {
							return fmt.Errorf("description-only change post-import: writes = %v, want exactly one PUT and zero ROTATE", w)
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    webhookHCL("https://x.io/h", []string{"cluster.ready"}, `description = "d2"`, `rotate_secret = "1"`),
				Check: resource.ComposeTestCheckFunc(
					resource.TestMatchResourceAttr(webhookResource, "secret", webhookSecretRE),
					func(*terraform.State) error {
						if w := webhookWrites(api, seen); strings.Join(w, ",") != "ROTATE" {
							return fmt.Errorf("post-import rotate_secret set: writes = %v, want exactly one ROTATE", w)
						}
						return nil
					},
				),
			},
		},
	})
}

// ---------------------------------------------------------------- 17

// Fails if a 404 on Read (webhook removed out of band) leaves the resource
// in state, or errors instead of recreating: the next plan must recreate it.
func TestWebhookResource_Read404RemovesFromStateAndNextPlanRecreates(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: webhookHCL("https://x.io/h", []string{"cluster.ready"}),
				Check: func(s *terraform.State) error {
					id = attrs(s, webhookResource)["id"]
					return nil
				},
			},
			{
				PreConfig: func() { api.RemoveWebhook(testOrg, id) },
				Config:    webhookHCL("https://x.io/h", []string{"cluster.ready"}),
				Check: func(*terraform.State) error {
					if posts := requestsMatching(api, http.MethodPost, webhooksListRE); len(posts) != 2 {
						return fmt.Errorf("want the original POST plus the recreate's, got %d", len(posts))
					}
					return nil
				},
			},
		},
	})
}

// Fails if Read (or the whole apply) misreports a 403 (wrong org for this
// token) as gone: it must be an error, with state kept.
func TestWebhookResource_Read403IsAnErrorAndKeepsState(t *testing.T) {
	api := setupFake(t)
	var route string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: webhookHCL("https://x.io/h", []string{"cluster.ready"}),
				Check: func(s *terraform.State) error {
					route = "GET /organizations/" + testOrg + "/webhooks/" + attrs(s, webhookResource)["id"]
					return nil
				},
			},
			{
				PreConfig: func() {
					api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
						fakeapi.WriteJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "forbidden", "message": "no access to this organization for this token"}})
					}
				},
				Config:      webhookHCL("https://x.io/h", []string{"cluster.ready"}),
				ExpectError: loose("no access to this organization for this token"),
			},
			{
				PreConfig: func() { delete(api.Handlers, route) },
				Config:    webhookHCL("https://x.io/h", []string{"cluster.ready"}), PlanOnly: true, ExpectNonEmptyPlan: false,
			},
		},
	})
}

// ---------------------------------------------------------------- 18

// Fails if destroy sends anything but exactly one DELETE.
func TestWebhookResource_DestroySendsExactlyOneDelete(t *testing.T) {
	api := setupFake(t)
	var seen int
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{Config: webhookHCL("https://x.io/h", []string{"cluster.ready"})},
			{
				PreConfig: func() { seen = len(api.Requests()) },
				Config:    providerOnlyHCL,
				Check: resource.ComposeTestCheckFunc(
					checkAbsentFromState(webhookResource),
					func(*terraform.State) error {
						if w := webhookWrites(api, seen); strings.Join(w, ",") != "DELETE" {
							return fmt.Errorf("destroy did %v, want exactly one DELETE", w)
						}
						return nil
					},
				),
			},
		},
	})
}

// Fails if Delete treats the API's 404 (already removed out of band, exactly
// on the delete request itself) as an error: the destroy must succeed.
func TestWebhookResource_DeleteOfAnAlreadyRemovedWebhookIsSuccess(t *testing.T) {
	api := setupFake(t)
	var id string
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config: webhookHCL("https://x.io/h", []string{"cluster.ready"}),
				Check: func(s *terraform.State) error {
					id = attrs(s, webhookResource)["id"]
					return nil
				},
			},
			{
				// The refresh sees the webhook (so Delete runs); the DELETE call
				// itself removes it out of band FIRST and then answers 404 - exactly
				// a removal between the refresh and the delete.
				PreConfig: func() {
					api.Handlers["DELETE /organizations/"+testOrg+"/webhooks/"+id] = func(w http.ResponseWriter, r *http.Request) {
						api.RemoveWebhook(testOrg, id)
						fakeapi.WriteJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "Resource not found"}})
					}
				},
				Config: providerOnlyHCL,
				Check:  checkAbsentFromState(webhookResource),
			},
		},
	})
}

// ---------------------------------------------------------------- review r1 M-2

// Fails on the CURRENT resource: a 201 with JSON201 != nil but Secret == nil
// (unreachable against the measured API, contract §2 - every real 201 carries
// the secret - but possible from a proxy/mock in front of it) makes Create
// return an error WITHOUT ever calling resp.State.Set (R:243-247 as reviewed).
// The webhook the fake really created (a real, GETable/DELETE-able row - this
// override seeds it for real via SeedWebhook, then echoes its JSON with the
// `secret` key stripped) is then ORPHANED: nothing is recorded in state, so
// the id is lost, and the next apply just tries to create a SECOND webhook
// instead of replacing the first (which needs a DELETE that only a saved id
// could send) - exactly the failure mode T10-3's own reasoning exists to
// prevent ("the webhook must not be lost").
//
// It stays RED until the coder's M-2 fix: on JSON201 != nil (secret or not),
// flatten and save state with a null secret, then add the error - tainting
// the resource so the next apply replaces it, mirroring test 3's shape.
func TestWebhookResource_Create201WithoutASecretStillSavesStateSoRecoveryReplaces(t *testing.T) {
	api := setupFake(t)
	var seededID string
	route := "POST /organizations/" + testOrg + "/webhooks"
	api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
		seededID = api.SeedWebhook(testOrg, "https://example.com/hook", []string{"cluster.ready"}, true)
		fakeapi.WriteJSON(w, http.StatusCreated, map[string]any{
			"id": seededID, "org_id": testOrg, "url": "https://example.com/hook",
			"description": "", "events": []string{"cluster.ready"}, "enabled": true,
			"created_at": "2026-09-28T00:00:00Z", "updated_at": "2026-09-28T00:00:00Z",
			// no "secret" key: the M-2 shape.
		})
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config:      webhookHCL("https://example.com/hook", []string{"cluster.ready"}),
				ExpectError: regexp.MustCompile(`(?is)secret`),
			},
			{
				PreConfig: func() { delete(api.Handlers, route) },
				Config:    webhookHCL("https://example.com/hook", []string{"cluster.ready"}),
				Check: func(*terraform.State) error {
					deletes := requestsMatching(api, http.MethodDelete, webhookOneRE)
					if len(deletes) != 1 || !strings.HasSuffix(deletes[0].Path, "/"+seededID) {
						return fmt.Errorf("recovery apply's DELETEs = %v, want exactly one targeting %s - proof state was saved with the id despite the missing secret (M-2)", deletes, seededID)
					}
					if posts := requestsMatching(api, http.MethodPost, webhooksListRE); len(posts) != 2 {
						return fmt.Errorf("want 2 POSTs total (the M-2 create plus the replacement), got %d: %v", len(posts), posts)
					}
					return nil
				},
			},
		},
	})
}
