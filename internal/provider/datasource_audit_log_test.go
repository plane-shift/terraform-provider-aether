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
