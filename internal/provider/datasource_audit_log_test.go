package provider_test

// Tests for the aether_audit_log data source (datasource_audit_log.go, T14-8),
// registered as NewAuditLogDataSource in provider.go's DataSources().

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/plane-shift/terraform-provider-aether/internal/provider/fakeapi"
)

const auditLogDataSource = "data.aether_audit_log.a"

func auditLogHCL(extra ...string) string {
	s := ""
	for _, e := range extra {
		s += "\n  " + e
	}
	return fmt.Sprintf(`
provider "aether" {}
data "aether_audit_log" "a" {%s
}
`, s)
}

// AL1. Seeds 10 events (more than limit=7) and checks: exactly `limit=7` is
// sent, exactly ONE audit-log request is made (the data source must NOT walk
// pages - contract §3's cursor is broken, so "just page anyway" would be
// silently wrong), the result is newest-first, and metadata_json is a
// COMPACT JSON string.
func TestAuditLogDataSource_OnePageNewestFirstCompactMetadata(t *testing.T) {
	api := setupFake(t)
	var lastID string
	for i := 0; i < 10; i++ {
		lastID = api.SeedAuditEvent(testOrg, fakeapi.AuditEventSeed{
			ActorType: "system", Action: fmt.Sprintf("action-%02d", i), ResourceType: "cluster",
			Metadata: map[string]any{"n": i},
		})
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: auditLogHCL("limit = 7"),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(auditLogDataSource, "limit", "7"),
				resource.TestCheckResourceAttr(auditLogDataSource, "events.#", "7"),
				resource.TestCheckResourceAttr(auditLogDataSource, "events.0.id", lastID),
				resource.TestCheckResourceAttr(auditLogDataSource, "events.0.action", "action-09"),
				resource.TestCheckResourceAttr(auditLogDataSource, "events.0.actor_type", "system"),
				resource.TestCheckResourceAttr(auditLogDataSource, "events.0.resource_type", "cluster"),
				resource.TestCheckResourceAttrSet(auditLogDataSource, "events.0.created_at"),
				func(s *terraform.State) error {
					a := attrs(s, auditLogDataSource)
					md := a["events.0.metadata_json"]
					if md != `{"n":9}` {
						return fmt.Errorf("events.0.metadata_json = %q, want the compact JSON string %q", md, `{"n":9}`)
					}
					return nil
				},
				func(*terraform.State) error {
					gets := requestsMatching(api, http.MethodGet, `/audit-log$`)
					if len(gets) != 1 {
						return fmt.Errorf("GET /audit-log calls = %d, want exactly 1 (the data source must fetch ONE page only, not walk the broken cursor)", len(gets))
					}
					if !hasQueryPair(gets[0].Query, "limit", "7") {
						return fmt.Errorf("GET query = %q, want limit=7", gets[0].Query)
					}
					return nil
				},
			),
		}},
	})
}

// Fails if actor_id/actor_email/resource_id/ip_address/metadata_json are not
// all null when the underlying event carries none of them (bare event).
func TestAuditLogDataSource_OptionalFieldsNullWhenAbsent(t *testing.T) {
	api := setupFake(t)
	api.SeedAuditEvent(testOrg, fakeapi.AuditEventSeed{ActorType: "system", Action: "bare", ResourceType: "cluster"})
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: auditLogHCL(),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckNoResourceAttr(auditLogDataSource, "events.0.actor_id"),
				resource.TestCheckNoResourceAttr(auditLogDataSource, "events.0.actor_email"),
				resource.TestCheckNoResourceAttr(auditLogDataSource, "events.0.resource_id"),
				resource.TestCheckNoResourceAttr(auditLogDataSource, "events.0.ip_address"),
				resource.TestCheckNoResourceAttr(auditLogDataSource, "events.0.metadata_json"),
			),
		}},
	})
}

// AL2. `limit` outside 1..100 must be rejected AT PLAN TIME by the schema's
// own Between(1,100) validator (T14-8) - never sent to the fake as an
// out-of-range value for the fake to silently coerce.
func TestAuditLogDataSource_LimitOutOfRangeIsPlanError(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      auditLogHCL("limit = 101"),
			ExpectError: loose("value must be between 1 and 100"),
		}},
	})
}

// AL2 (the other bound).
func TestAuditLogDataSource_LimitZeroIsPlanError(t *testing.T) {
	setupFake(t)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      auditLogHCL("limit = 0"),
			ExpectError: loose("value must be between 1 and 100"),
		}},
	})
}

// Coder's suggested pin 1 (task-14-report.md "Fix round 2", review r1 M-4):
// a 200 body carrying a field the data source never exposes (`org_id`) with a
// value that would fail the generated client's TYPED decode (a non-UUID
// string into openapi_types.UUID - client.gen.go's AuditEvent.OrgId is a
// required, non-pointer UUID) must still be read successfully, because
// T14-17's raw decode into auditEventWire never even looks at org_id. If the
// data source regressed to ListAuditLogWithResponse, this read would fail
// with a JSON/UUID decode error instead of succeeding.
func TestAuditLogDataSource_UnexposedFieldWithOddValueIsIgnored(t *testing.T) {
	api := setupFake(t)
	route := "GET /organizations/" + testOrg + "/audit-log"
	api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
		fakeapi.WriteJSON(w, http.StatusOK, map[string]any{
			"items": []map[string]any{
				{
					"id":            "11111111-1111-4111-8111-111111111111",
					"org_id":        "not-a-uuid", // unexposed; would break a typed decode
					"actor_type":    "system",
					"action":        "test.action",
					"resource_type": "cluster",
					"created_at":    "2026-09-28T00:00:00Z",
				},
			},
		})
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: auditLogHCL(),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(auditLogDataSource, "events.#", "1"),
				resource.TestCheckResourceAttr(auditLogDataSource, "events.0.id", "11111111-1111-4111-8111-111111111111"),
				resource.TestCheckResourceAttr(auditLogDataSource, "events.0.action", "test.action"),
			),
		}},
	})
}

// M-6: a non-200 from the audit-log route must surface as a plan-time error
// carrying the status and message.
func TestAuditLogDataSource_APIErrorSurfaces(t *testing.T) {
	api := setupFake(t)
	route := "GET /organizations/" + testOrg + "/audit-log"
	api.Handlers[route] = func(w http.ResponseWriter, r *http.Request) {
		fakeapi.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]any{"code": "internal", "message": "audit log store unavailable"}})
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config:      auditLogHCL(),
			ExpectError: loose("500 internal: audit log store unavailable"),
		}},
	})
}

// Fails if `limit` omitted does not default to 50 (T14-8), if the effective
// value 50 is not written into state (review r1 M-3: an Optional+Computed
// attribute must echo what it actually used, not just leave the config null),
// or if the request itself did not carry limit=50 (M-3's remaining gap - this
// test would also pass if the provider sent no limit at all and the fake's
// own default happened to be 50, unless the query is checked directly).
func TestAuditLogDataSource_LimitDefaultsToFifty(t *testing.T) {
	api := setupFake(t)
	for i := 0; i < 60; i++ {
		api.SeedAuditEvent(testOrg, fakeapi.AuditEventSeed{ActorType: "system", Action: "a", ResourceType: "r"})
	}
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: auditLogHCL(),
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(auditLogDataSource, "limit", "50"),
				resource.TestCheckResourceAttr(auditLogDataSource, "events.#", "50"),
				func(*terraform.State) error {
					gets := requestsMatching(api, http.MethodGet, `/audit-log$`)
					if len(gets) != 1 {
						return fmt.Errorf("GET /audit-log calls = %d, want exactly 1", len(gets))
					}
					if !hasQueryPair(gets[0].Query, "limit", "50") {
						return fmt.Errorf("GET query = %q, want limit=50", gets[0].Query)
					}
					return nil
				},
			),
		}},
	})
}
