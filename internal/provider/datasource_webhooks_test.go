package provider_test

// Tests for the aether_webhooks data source (not yet implemented -
// datasource_webhooks.go, T14-2). Until NewWebhooksDataSource is registered
// in provider.go's DataSources(), every test here fails at plan time with
// "Invalid Data Source Type" (see datasource_organization_members_test.go's
// header comment for the full explanation).

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// checkAbsentFromStateSubstring fails if ANY attribute key of addr contains
// substr, case-insensitively - a belt-and-braces check that a secret cannot
// leak under a differently-named attribute (e.g. "webhook_secret" instead of
// the expected, and deliberately absent, "secret").
func checkAbsentFromStateSubstring(addr, substr string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for k := range attrs(s, addr) {
			if strings.Contains(strings.ToLower(k), strings.ToLower(substr)) {
				return fmt.Errorf("%s has attribute key %q, which contains %q - want no such key", addr, k, substr)
			}
		}
		return nil
	}
}

const webhooksDataSource = "data.aether_webhooks.w"

const webhooksDSHCL = `
provider "aether" {}
data "aether_webhooks" "w" {}
`

// Fails if any of id/url/description/events/enabled/created_at/updated_at is
// missing, if events is not exposed as a Set (order-independent membership),
// or - the point of WH1 - if `secret` is present anywhere in state (the list
// route never returns it, contract §10/T14-2).
func TestWebhooksDataSource_ReadAllAttributesNoSecret(t *testing.T) {
	api := setupFake(t)
	id := api.SeedWebhook(testOrg, "https://example.com/hook", []string{"cluster.created", "cluster.deleted"}, true)
	resource.UnitTest(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{{
			Config: webhooksDSHCL,
			Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr(webhooksDataSource, "webhooks.#", "1"),
				resource.TestCheckResourceAttr(webhooksDataSource, "webhooks.0.id", id),
				resource.TestCheckResourceAttr(webhooksDataSource, "webhooks.0.url", "https://example.com/hook"),
				resource.TestCheckResourceAttr(webhooksDataSource, "webhooks.0.description", ""),
				resource.TestCheckResourceAttr(webhooksDataSource, "webhooks.0.events.#", "2"),
				resource.TestCheckTypeSetElemAttr(webhooksDataSource, "webhooks.0.events.*", "cluster.created"),
				resource.TestCheckTypeSetElemAttr(webhooksDataSource, "webhooks.0.events.*", "cluster.deleted"),
				resource.TestCheckResourceAttr(webhooksDataSource, "webhooks.0.enabled", "true"),
				resource.TestCheckResourceAttrSet(webhooksDataSource, "webhooks.0.created_at"),
				resource.TestCheckResourceAttrSet(webhooksDataSource, "webhooks.0.updated_at"),
				resource.TestCheckNoResourceAttr(webhooksDataSource, "webhooks.0.secret"),
				checkAbsentFromStateSubstring(webhooksDataSource, "secret"),
			),
		}},
	})
}
